# Claworc-managed OpenBao — scoping + implementation plan

**Author:** Clawmaster
**Date:** 2026-09-01
**Status:** Approved by Shaun 2026-09-01 — implementing now
**Scope:** Claworc runs and owns a single self-hosted OpenBao instance as a managed platform service (same shape as `internal/connectorprov`), auto-unseals it on boot, and provisions per-agent long-lived tokens scoped to that agent's own secret path plus admin-configured named shared secret sets.

---

## 1. Decisions (from discussion with Shaun, 2026-09-01)

| Question | Decision |
|---|---|
| Topology | Single OpenBao container, Claworc-managed, one per Claworc install (not per-team, not per-agent) |
| Storage backend | `file` storage backend on its own volume. Not Postgres. |
| Network | Loopback/bridge-network only, same trust boundary as the connector container. No TLS between Claworc and OpenBao (internal traffic only). |
| Unseal | Shamir `secret_shares=1`, `secret_threshold=1`. Claworc holds the single unseal key, encrypted at rest, and auto-unseals on every OpenBao container start/restart — no human step. |
| Root token | **Kept**, encrypted at rest, for recovery purposes — even though day-to-day API calls use a separate non-root admin token minted from a dedicated policy. Explicit reversal of the original "discard root token" proposal. |
| Path layout | Single KV v2 mount at `secret/`. `secret/agents/<instance-uuid>/**` per agent (implicit, always RW, not user-configurable). `secret/shared/<set-name>/**` for admin-created named sets. |
| Own-namespace access | Always full read+write. Not part of the grants list; never shown as a grant. |
| Shared sets | Admin-created/deleted ad hoc from the Claworc UI, arbitrary name, no fixed limit. Pure `{name}` records — the path prefix is derived from the name. |
| Per-agent grant field | New instance-level config field: list of `{set_name, capability: "read"|"write"}`. Default capability when a set is granted with no explicit choice: **read**. Write is an explicit opt-in per grant. |
| Token lifetime | **Long-lived.** No periodic renewal, no short TTL. Minted once (or on first policy change if none exists yet) with a large explicit TTL (10 years) since OpenBao does not have a true "infinite" token concept — periodic tokens or default system max-TTL would otherwise silently expire it. |
| Token delivery | Two consumption paths from one token: (1) `BAO_ADDR` + `BAO_TOKEN` env vars into the container for direct script/`bao`-CLI use -- these are the names the CLI reads, and are the only pair injected (v1 shipped `OPENBAO_ADDR`/`OPENBAO_TOKEN`, which the CLI ignores); (2) an OpenClaw `secrets.providers.openbao` exec-provider config block for SecretRef-eligible fields — **deferred to a fast-follow**, see §6, because it requires a resolver script baked into the agent image, which is a different repo/pipeline than control-plane wiring. |
| Policy vs token on grant change | Token stays stable; only the OpenBao **policy** attached to it is rewritten when `SecretGrants` changes. |
| Revocation on instance delete | **Leave-be for now.** No revoke, no orphan sweep. Matches the connector token's current stance before its own sync work landed. |
| Audit logging | Out of scope. Not wired up. |
| Migration of existing secrets | None. This is new-secrets-only; Brave key, connector tokens, GitHub PAT etc. stay where they are. |

## 2. Architecture

Mirrors `internal/connectorprov` + `internal/handlers/connector.go` almost exactly, since it is the same class of problem ("Claworc runs one shared backing service and mints per-instance scoped credentials against it"):

```
internal/openbaoprov/
  openbaoprov.go      WorkloadSpec builder, Manager{Apply,Delete,Status,Address,WaitHealthy}
  admin_client.go      HTTP client for OpenBao's own API (init/unseal/mounts/policies/tokens/kv)
internal/handlers/
  openbao.go           settings glue, ensureOpenbaoInitialized, applyOpenbaoAsync,
                       per-instance token/policy provisioning, env injection
  openbao_shared_sets.go  CRUD for named shared secret sets (Settings UI-driven)
  openbao_secrets.go      per-agent secret browse/set/reveal/delete within one
                          instance's own agents/<uuid>/ namespace (Agent detail UI)
```

### 2.1 Workload

`WorkloadSpec{Name: "claworc-openbao", Image: openbao_image, ...}`, built and applied through the same `orch.Apply()` primitive the connector uses. No SSH tunnel needed (plain HTTP like the connector, not loopback-only CDP/VNC like the browser).

Config is supplied via the `BAO_LOCAL_CONFIG` env var (confirmed supported by the official `openbao/openbao` image — JSON config passed inline, no file mount needed):

```json
{
  "storage": { "file": { "path": "/openbao/data" } },
  "listener": { "tcp": { "address": "0.0.0.0:8200", "tls_disable": true } },
  "disable_mlock": true,
  "api_addr": "http://claworc-openbao:8200"
}
```

Volume: `claworc-openbao-data` → `/openbao/data`. Default image: `openbao/openbao:latest` (plain setting `openbao_image`, same "mutable default, admin can pin" pattern as `connector_image`).

### 2.2 Init / unseal lifecycle

On `applyOpenbaoAsync` after the workload is healthy (`/v1/sys/health` reachable):

1. Check seal status (`GET /v1/sys/seal-status`).
2. If never initialized (`initialized: false`): `PUT /v1/sys/init` with `{"secret_shares":1,"secret_threshold":1}`. Response gives `root_token` + `keys[0]`. Encrypt and store both (`openbao_root_token`, `openbao_unseal_key`) via the existing `fixedEncryptedSettings` mechanism (`utils.Encrypt`), same as `connector_admin_token`.
3. If sealed (`sealed: true`): `PUT /v1/sys/unseal` with the stored key. Idempotent — safe to call on every apply/boot.
4. If no admin token minted yet (`openbao_admin_token` setting empty): using the (now-unsealed) root token once, `PUT /v1/sys/policies/acl/claworc-admin` with a policy granting full CRUD on `secret/data/*` and `secret/metadata/*` and on `sys/policies/acl/*` (so Claworc can manage per-agent policies going forward), then `POST /v1/auth/token/create-orphan` with that policy and a long TTL, store the resulting token as `openbao_admin_token`. Root token is **not** discarded (per Shaun's decision) but is not used again in normal operation after this bootstrap step — every subsequent Claworc→OpenBao call uses the admin token.
5. Enable the KV v2 secret engine at `secret/` if not already mounted (`GET /v1/sys/mounts`, `POST /v1/sys/mounts/secret` with `{"type":"kv-v2"}` if missing).

This whole sequence is idempotent and safe to re-run on every control-plane boot when `openbao_enabled`, mirroring `applyConnectorAsync`'s shape.

### 2.3 Per-agent provisioning

On instance create, and on any edit to `SecretGrants`:

1. Compute the desired policy document for the instance from:
   - Always: full capabilities (`create,read,update,delete,list`) on `secret/data/agents/<instance-uuid>/*` and `secret/metadata/agents/<instance-uuid>/*`.
   - Per grant: `read,list` (capability `"read"`) or `create,read,update,delete,list` (capability `"write"`) on `secret/data/shared/<set_name>/*`, plus `read,list` on `secret/metadata/shared/<set_name>/*` in both cases (list/metadata access is needed to enumerate keys even for read-only grants).
2. `PUT /v1/sys/policies/acl/agent-<instance-uuid>` with that document (upsert — always safe to overwrite).
3. If the instance has no token yet (`Instance.OpenbaoToken` empty): `POST /v1/auth/token/create-orphan` with `{"policies":["agent-<instance-uuid>"],"ttl":"87600h","renewable":false}`, store the resulting token encrypted on the instance row. TTL is long (10 years) rather than literally infinite because OpenBao token creation doesn't have a true no-expiry mode outside the root token itself.
4. If the instance already has a token: nothing to do — the policy attached to it was just rewritten in step 2, and OpenBao re-evaluates a token's effective grants from its attached policies on every request, so no token rotation is needed when only the grant set changes.

### 2.4 Env injection

`buildOpenbaoEnvVars(inst)` (same shape as `buildConnectorEnvVars`), called from the same site in `instances.go` that injects `CLAWORC_INSTANCE_ID` / connector vars:

```go
BAO_ADDR      = "http://claworc-openbao:8200"
BAO_TOKEN     = <decrypted per-instance token>
```

Wired into `EnsureEnvPropagated`'s drift check the same way connector env vars are, so enabling the feature (or granting a new shared set) on an already-running instance restarts it to pick up the token if the token is newly minted, but does **not** restart on a pure policy change (the token value itself is unchanged — OpenBao re-evaluates policy server-side, no container-level drift).

## 3. Data model changes

```go
// New model
type SharedSecretSet struct {
    ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
    Name      string    `gorm:"uniqueIndex;not null" json:"name"` // used verbatim as the secret/shared/<name>/ path segment
    CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
    UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// Instance additions
SecretGrants string `gorm:"type:text;default:'[]'" json:"secret_grants"` // JSON []SecretGrant{SetName string; Capability string}
OpenbaoToken string `json:"-"` // fernet-encrypted at rest, mirrors ConnectorRuntimeToken
```

`Name` validation: same charset constraint OpenBao path segments tolerate cleanly — restrict to `^[a-z][a-z0-9-]{0,62}$` at the Claworc API layer (defensive; not strictly required by OpenBao itself, but keeps paths predictable and avoids surprises with reserved characters).

Both additions are additive columns/tables — no numbered goose migration needed, just add `&models.SharedSecretSet{}` to `AutoMigrateAll`'s list (same as how `BrowserSession`/`Team`/etc. were added over time).

## 4. Settings additions

`plainSettings`: `openbao_enabled`, `openbao_image`, `openbao_storage`.
`fixedEncryptedSettings`: `openbao_root_token`, `openbao_unseal_key`, `openbao_admin_token`.

Toggle semantics mirror `connector_enabled` exactly: turning it on triggers `ensureOpenbaoInitialized` + `applyOpenbaoAsync(true)`; turning it off calls `orch.StopInstance` (workload stopped, volume retained, root token/unseal key/admin token all kept in settings so re-enabling doesn't need re-init).

## 5. UI touch points

- **Settings page**: new card (`OpenbaoSettings.tsx`, same shape as `ApiKeySettings.tsx`/connector's settings card) — enable toggle, image field, masked root-token/admin-token display (`has_override`-style, matching Brave key's masked-value convention — never shown in plaintext after generation). A small "Shared secret sets" management list (create-by-name, delete) lives here too.
- **Agent form** (`AgentForm.tsx`): new "Secret grants" section — for each existing `SharedSecretSet`, a read/write/none selector. Renders to `Instance.SecretGrants` JSON on save.
- **Instance detail**: optional small indicator "OpenBao: connected" once a token exists, matching the connector's per-instance indicator — nice-to-have, not blocking.
- **Agent detail — secrets panel** (`InstanceSecretsPanel.tsx`, below the grant editor): lists every secret in that one agent's own `agents/<uuid>/` namespace with field names and masked values, and sets a single field on any path, creating the secret if it does not exist. Reveal and copy fetch one plaintext value at a time through a separate endpoint (`GET /instances/{id}/secrets/reveal`), each logged, so a list response never carries plaintext. Admin-only, and every path is built by prefixing the instance's own namespace, so the endpoint cannot address another agent's secrets or a shared set. Calls go out over Claworc's own admin token, not the agent's, so an admin can seed a secret for an agent that is stopped or has never booted.

## 6. Explicitly deferred (fast-follow, not v1)

- **SecretRef exec-provider wiring** (`secrets.providers.openbao` block rendered into `openclaw.json` pointing at a `bao`-CLI-compatible resolver): the `bao` CLI binary is now baked into the agent image (`agent/instance/Dockerfile`, pinned via `OPENBAO_VERSION`, downloaded from the `openbao/openbao` GitHub release matching `TARGETARCH`) — v1 shipped `OPENBAO_ADDR`/`OPENBAO_TOKEN`, which the CLI does not read; those are retired in favour of `BAO_ADDR`/`BAO_TOKEN` so any script/exec-tool call can use the `bao` CLI directly (`bao kv get ...`) without an operator installing it themselves. Agents need no path configuration: `bao read sys/internal/ui/resultant-acl` returns the token's complete effective ACL, covering both its own `secret/agents/<uuid>/` namespace and any granted shared sets. Still outstanding: the control-plane side — rendering a `secrets.providers.openbao` exec-provider block into `openclaw.json` so OpenClaw's own SecretRef-eligible config fields (model API keys etc.) can resolve against OpenBao.
- Revocation on instance delete, orphaned-token sweep on startup.
- Audit log wiring.
- Kubernetes-backend-specific parity review (Service/NetworkPolicy naming) — expected to fall out mostly for free via `Apply()`, same as the connector's Level 2, but not explicitly verified here.
- Migrating any existing plaintext secrets into OpenBao.

## 7. File/code pointers

- `control-plane/internal/connectorprov/` — structural precedent, copied almost directly.
- `control-plane/internal/handlers/connector.go` — settings glue, async apply, per-instance token minting precedent.
- `control-plane/internal/orchestrator/spec.go`, `docker_apply.go` — `WorkloadSpec`/`Apply()`.
- `control-plane/internal/handlers/env_propagation.go` — `EnsureEnvPropagated` drift-restart mechanism.
- `control-plane/internal/database/models/models.go` — `Instance`, `SharedFolder` for field-shape precedent.
- `control-plane/internal/database/migrations/migration_00001_baseline.go` — `AutoMigrateAll` model list.
- `control-plane/internal/utils/crypto.go` — `Encrypt`/`Decrypt`/`Mask`.
- OpenBao HTTP API (Vault-API-compatible): `/v1/sys/init`, `/v1/sys/seal-status`, `/v1/sys/unseal`, `/v1/sys/mounts`, `/v1/sys/policies/acl/:name`, `/v1/auth/token/create-orphan`, `/v1/secret/data/:path` (KV v2 read/write).
- Confirmed via Docker Hub docs: official `openbao/openbao` image accepts inline JSON config via `BAO_LOCAL_CONFIG` env var — no config-file volume mount required.
