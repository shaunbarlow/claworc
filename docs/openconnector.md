# Managed OpenConnector (Level 1)

## What this is

Claworc can run and manage a single, shared [OpenConnector](https://github.com/shaunbarlow/open-connector)
(OOMOL Connect) service that every agent instance can reach for third-party
provider actions (arxiv, better_stack_telemetry, aws, ...). This is "Level 1"
from `docs/planning/open-connector-integration-plan.md`: one always-on
deployment, not a per-instance workload, with per-agent access controlled by
individually minted, scoped runtime tokens rather than one shared secret
pasted into every instance's env vars.

## Enabling it

Settings → Misc → **Managed OpenConnector** → toggle **Enabled**.

On first enable, Claworc:

1. Generates `connector_encryption_key` (32 random bytes, base64) and
   `connector_admin_token` (32 random bytes, hex), storing both
   Fernet-encrypted in the `settings` table — the same storage and masking
   convention as `brave_api_key`. These secrets are never rendered into any
   agent's config or environment; they exist only for the control plane's own
   admin-API calls to the connector.
2. Applies a `WorkloadSpec` for the connector container/Deployment via the
   active orchestrator's generic `Apply()` primitive (the same mechanism the
   on-demand browser feature uses — see `docs/ondemand-browser.md` and
   `internal/browserprov`), publishing its HTTP port so the control plane can
   reach it directly for admin-API calls even when it runs outside Docker.
3. Waits for `GET /v1/health` to return 200, then mints a scoped runtime
   token for every existing instance and restarts any running instance whose
   container is missing `OOMOL_CONNECT_RUNTIME_TOKEN` /
   `OPEN_CONNECTOR_BASE_URL` — mirroring the env-var drift-restart cascade
   `UpdateSettings` already runs for global env-var and Brave-key changes
   (see `docs/env-propagation.md`).

Disabling the toggle stops the container (`StopInstance`) but leaves its data
volume and secrets in place, so re-enabling later does not need to
re-provision every instance's token from scratch.

## What an agent gets

Every instance created or restarted while the feature is enabled receives:

- `OPEN_CONNECTOR_BASE_URL` — `http://claworc-connector:3000`, the connector's
  stable in-network name (Docker bridge / Kubernetes Service DNS — agents and
  the connector always share the same network, so this hostname resolves the
  same way on both backends).
- `OOMOL_CONNECT_RUNTIME_TOKEN` — a runtime token scoped to that instance
  alone, minted via the connector's own admin API
  (`POST /api/runtime-tokens`) and stored Fernet-encrypted on the `Instance`
  row (`ConnectorRuntimeToken` / `ConnectorTokenID`).

Agents consume this exactly as documented in the `open-connector` skill
(`skills/open-connector/SKILL.md`): call the runtime API, never the admin
API, and treat `action_not_allowed` as the scoping working as intended.

Deleting an instance revokes its token (`DELETE /api/runtime-tokens/:id`)
so the connector's token list does not accumulate orphans.

## Dashboard access

The connector's own Web Console + admin API is reachable at `/connector/*`
on Claworc's own origin — the same "no separate port, no separate URL"
forwarding path the per-instance OpenClaw dashboard uses at `/openclaw/{id}/*`
(see `ControlProxy` in `internal/handlers/control.go`). `ConnectorProxy`
injects the connector's own admin bearer token onto every proxied request, so
reaching it only requires being an authenticated Claworc admin — nobody needs
to know or copy the connector's own admin token by hand.

## Settings reference

| Setting | Type | Notes |
|---|---|---|
| `connector_enabled` | bool (as `"true"`/`"false"`) | Master switch; triggers Apply/Stop. |
| `connector_image` | string | Defaults to `ghcr.io/shaunbarlow/open-connector:tip`. |
| `connector_storage` | string | Data volume size, default `10Gi`. Honoured on first creation only. |
| `connector_origin` | string | Optional `OOMOL_CONNECT_ORIGIN` override for OAuth redirect URLs. |
| `connector_encryption_key` | encrypted | Auto-generated; never surfaced in plaintext after creation. |
| `connector_admin_token` | encrypted | Auto-generated; never surfaced in plaintext after creation. |

`connector_image` / `connector_storage` / `connector_origin` changes take
effect on the next `Apply` (toggle the feature off/on, or a control-plane
restart), matching how `default_container_image` only affects instances
created or restarted after the edit.

## API

- `GET /api/v1/connector/status` (admin) — `{ enabled, configured, status }`.
- `GET|POST /connector/*` (admin) — proxied straight through to the
  connector's own `/api/*` admin surface and Web Console.

## Not in scope for Level 1

- Kubernetes-specific parity beyond what `Apply()` already gives for free
  (PVC storage class, dedicated NetworkPolicy tuned to "agents dial the
  connector directly" vs. "control-plane relays" — see Open Question #1 in
  the integration plan) — Level 2.
- Per-agent policy configuration (allowedActions/allowedConnections) from the
  Claworc UI — every minted token today is unrestricted at the connector
  policy layer, same posture as OpenClaw's own env-var-sourced secrets.
  Narrowing this later is additive (`RuntimeTokenSpec` already accepts the
  fields; nothing about the storage or minting flow needs to change).
- Backup coverage for the connector's own data volume — track alongside
  `docs/backups.md` when needed.
