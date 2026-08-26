# Claworc ⇄ OpenConnector integration — scoping plan

**Author:** Clawmaster
**Date:** 2026-08-25 (Level 1 implemented 2026-08-26 on the `active` branch — see `docs/openconnector.md`)
**Status:** Level 1 shipped; Level 2 (Kubernetes-specific parity) and per-agent policy UI remain open
**Scope:** Evaluate spinning up `shaunbarlow/open-connector` (GHCR `tip` tag) from Claworc, and whether Claworc can lean on open-connector's built-in `docker compose` setup or needs its own orchestrator-driven wiring.

---

## TL;DR

- **Don't reuse `docker compose` directly.** Claworc's own `docker-compose.yml` is explicitly a reference example (comment: *"It is recommended to use install.sh or install.ps1"*) — real installs go through `install.sh` (raw `docker run`) or the Helm chart (Kubernetes), never through `docker compose up`. There's no `compose-go`/`docker compose` invocation anywhere in the control-plane codebase; the two Docker backends drive containers directly via the Docker Engine API (`docker.go`) or the Kubernetes API (`kubernetes.go`).
- **Open-connector should be modeled as a shared platform service**, not a per-agent-instance workload — one connector runtime backing every agent, with per-instance scoping done via open-connector's own runtime-token policy (`allowedActions`/`allowedConnections`), not by running N separate connector containers. This matches what's already true in *this* agent container today: `OOMOL_CONNECT_RUNTIME_TOKEN` / `OOMOL_CONNECT_ADMIN_TOKEN` are already present as env vars, implying a shared connector deployment model is the intended one.
- **The generic `WorkloadSpec` / `Apply()` primitive Claworc already built for the on-demand browser feature is the right integration point.** Both orchestrator backends (Docker, Kubernetes) already implement `Apply(ctx, WorkloadSpec) error` generically — reusing it for open-connector means no new orchestrator abstraction, just a new caller (a `connectorprov`-style package, mirroring `browserprov`).
- **Effort: Medium**, similar order of magnitude to the on-demand browser feature (`internal/browserprov`) that's already shipped. Docker-backend parity is the bulk of the work; Kubernetes parity is mostly "free" once `Apply()` is used, plus Helm/NetworkPolicy plumbing.
- Recommend **not** trying to make Claworc literally shell out to `docker compose` in the open-connector repo — it fights the existing single-orchestrator-API architecture and breaks Kubernetes deployments entirely.

---

## 1. Current state (evidence)

### 1.1 Claworc has no compose-based orchestration today
- `grep` across `control-plane/` for `compose-go`, `docker compose`, `dockercompose`, `compose.Project`, `loader.Load` → **no hits** except comments about *label compatibility* with `docker compose up` (`docker.go` `SelfUpdate`/`selfUpdateRunArgs`, `self_update_test.go`). Claworc reproduces `com.docker.compose.*` labels only so a compose-managed control-plane container isn't orphaned by self-update — it never itself calls `docker compose`.
- `install.sh` provisions the control-plane via `docker run` / Helm, not `docker compose up`.
- The repo's own root `docker-compose.yml` is explicitly commented as "an example. It is recommended to use install.sh or install.ps1."

**Conclusion:** there is no "builtin docker compose functionality in repo" to leverage for *orchestrating agent workloads*. The two supported backends are the Docker Engine API and the Kubernetes API, both behind `orchestrator.ContainerOrchestrator`.

### 1.2 Open-connector's own compose files (reference only)
`projects/open-connector/docker-compose.yml`:
```yaml
services:
  connector:
    image: ghcr.io/oomol-lab/open-connector:latest
    ports: ["3000:3000"]
    volumes: [connector-data:/app/data]
    environment:
      OOMOL_CONNECT_DATA_DIR: /app/data
      OOMOL_CONNECT_DATABASE_URL:
      OOMOL_CONNECT_ENCRYPTION_KEY:
      OOMOL_CONNECT_ADMIN_TOKEN:      # (redacted var name in captured file)
      OOMOL_CONNECT_RUNTIME_TOKEN:
      OOMOL_CONNECT_ALLOWED_ACTIONS: / OOMOL_CONNECT_BLOCKED_ACTIONS:
      OOMOL_CONNECT_ALLOWED_PROXIES: / OOMOL_CONNECT_BLOCKED_PROXIES:
      OOMOL_CONNECT_EGRESS_TRUSTED_HOSTS:
```
Plus `docker-compose.build.yml` as a build overlay (`docker/Dockerfile`) for local source builds.

This is genuinely useful **as documentation of the exact env var contract, image, port, and volume** — see `docs/docker-ghcr.md` in that repo. It is not something Claworc can `include:` or subprocess-invoke cleanly across repos, and doing so wouldn't work on the Kubernetes backend at all.

### 1.3 The reusable integration point: `WorkloadSpec` + `Apply()`
Claworc already solved "run a second, generically-described container/pod alongside an agent instance" once, for the on-demand browser feature (`internal/browserprov/local.go` + `internal/orchestrator/{docker_apply,kubernetes_apply}.go`):

- `orchestrator.WorkloadSpec` — name, image, env, volumes, empty-dirs, ports, probes, security, pull policy, init containers, labels, affinity. Backend-agnostic.
- `ContainerOrchestrator.Apply(ctx, spec)` — idempotent create-or-update; Docker backend creates a plain container + named volumes, Kubernetes backend creates/updates Deployment + PVC + Service + NetworkPolicy.
- `browserprov.LocalProvider` builds a `WorkloadSpec` from its own `SessionParams` and calls `orch.Apply(...)`. It also owns SSH-based readiness probing (`waitForCDPReady`), TOFU host-key handling, and volume-clone-on-agent-clone logic — most of that (SSH plumbing, host-key TOFU) is *not* needed for open-connector since it just needs an HTTP port reachable, not an SSH tunnel into a loopback-only service.

This is the natural shape for an `internal/connectorprov` (or similar) package: build a `WorkloadSpec` for the open-connector image, call `Apply()`, wait for `/health` (open-connector's own health endpoint, no SSH needed since HTTP:3000 can simply be published/reachable on the `claworc` bridge network or a ClusterIP Service — same pattern already used for the control-plane's own instance workloads).

### 1.4 Settings/plugin pattern already exists for "optional managed capability requiring its own image + secrets + npm/plugin install"
Two precedents to copy nearly verbatim:
- **Brave web search** (`internal/handlers/search.go`, `settings.go`): `default_search_provider` setting (`""` | `"brave"`), per-instance override (`Instance.SearchProvider`), a `searchProviderPluginSpecs` map from provider id → npm package, a global `brave_api_key` (Fernet-encrypted, `fixedEncryptedSettings`) with per-instance override, and `pushMemoryConfig`-style live SSH config push + `EnsureEnvPropagated`-based restart-on-drift.
- **lossless-claw context engine** (`internal/handlers/contextengine.go`): `default_context_engine` setting, a curated settings struct (`LosslessClawSettings`) mirrored between global default and per-instance override, JSON-schema-driven UI fields plus a raw JSON escape hatch.

Open-connector-as-a-managed-service is a third case of the same shape, but at the **deployment** layer (a whole extra container) rather than the **config** layer (a plugin entry inside an existing agent's `openclaw.json`). It combines both: Claworc needs to (a) run the connector container/pod, and (b) tell each agent instance how to reach it and with what token — the reach/token part is exactly the `open-connector` *skill* (`skills/open-connector/SKILL.md`, already present in this agent's workspace) which expects a base URL + a scoped runtime bearer token in env.

### 1.5 Env var injection pattern (how an instance gets told about it)
`internal/handlers/instances.go` already injects computed env vars into `CreateParams.EnvVars` at instance-create/update time (e.g. `CLAWORC_INSTANCE_ID`, `OPENCLAW_GATEWAY_TOKEN`, `OPENCLAW_INITIAL_SLACK/DISCORD/MODELS/PROVIDERS`). Adding `OOMOL_CONNECT_RUNTIME_TOKEN` + a base-URL var (e.g. `OPEN_CONNECTOR_BASE_URL`) for every instance is the same mechanism — no new plumbing required, just two more keys computed at the same call site.

### 1.6 GHCR image confirmed reachable
Verified via GitHub package page: `ghcr.io/shaunbarlow/open-connector:tip` exists, is public (no sign-in required to view), multi-arch is inherited from the fork's own `publish-docker.yml` workflow (native amd64+arm64 build, pushed on every commit to `active`). No pull secret needed unless the fork's package visibility changes.

---

## 2. Two integration levels, sized separately

### Level 0 — "Just let an operator run it next to Claworc manually" (docs-only)
Add a documented, optional service block operators can append to their own `docker-compose.yml`/`docker run` setup, or a Helm values snippet, pointing at `ghcr.io/shaunbarlow/open-connector:tip`, with the exact env-var contract from `docs/docker-ghcr.md`. Claworc does not manage its lifecycle; the operator sets `OPEN_CONNECTOR_BASE_URL`/`OOMOL_CONNECT_RUNTIME_TOKEN` by hand via the existing per-instance/global env-var UI (`envvars.go` / `default_env_vars` setting) — no code changes at all.

**Effort: trivial** (a docs page + maybe a sample compose snippet). No code. This is available today with zero Claworc changes — the `open-connector` skill already assumes exactly this shape (base URL + runtime token in env, provisioned by "an admin", per its own SKILL.md).

### Level 1 — Claworc provisions and manages a single shared open-connector service (Docker backend)
Give Claworc an actual "OpenConnector" managed-service concept, analogous to browser-on-demand but always-on/singleton rather than per-instance:

1. **Settings** (`settings.go` pattern):
   - `connector_enabled` (bool)
   - `connector_image` (plain setting, default `ghcr.io/shaunbarlow/open-connector:tip` or an org-level GHCR mirror)
   - `connector_encryption_key`, `connector_admin_token` — generated once, Fernet-encrypted at rest (`fixedEncryptedSettings`), same pattern as `brave_api_key`; **never** rendered to any agent instance's config, only used by the control-plane's own admin-API client (see step 3).
   - Optional `connector_database_url` for Postgres instead of the SQLite default.
2. **Lifecycle** (`internal/connectorprov`, modeled on `browserprov`):
   - `WorkloadSpec{Name: "claworc-connector", Image: connector_image, Env: {OOMOL_CONNECT_DATA_DIR, OOMOL_CONNECT_ENCRYPTION_KEY, OOMOL_CONNECT_ADMIN_TOKEN, OOMOL_CONNECT_ORIGIN, ...}, Volumes: [{Name: "claworc-connector-data", MountPath: "/app/data"}], Ports: [{Name: "http", ContainerPort: 3000}]}`.
   - Call `orch.Apply(ctx, spec)` on control-plane startup (if `connector_enabled`) and whenever `connector_image`/secrets change, exactly like `pushMemoryConfig`/`searchConfigSSHWait` re-push on settings change.
   - Readiness: poll `GET http://claworc-connector:3000/health` (Docker: reachable via the shared `claworc` bridge network by container name; K8s: via the ClusterIP Service DNS name) instead of the SSH-tunnel dance `browserprov` needs — open-connector exposes HTTP directly, no loopback-only CDP/VNC constraint driving the SSH design.
3. **Per-instance runtime token provisioning**: a small admin-API client (`POST {connector_base_url}/api/runtime-tokens` with the admin token) to mint a scoped token (`allowedActions`, `allowedConnections` as configured) at instance-create time, store it encrypted on the `Instance` row (mirrors `GatewayToken`/`BraveAPIKey` fields), and inject it as `OOMOL_CONNECT_RUNTIME_TOKEN` + `OPEN_CONNECTOR_BASE_URL` into the instance's env vars (same call site as `CLAWORC_INSTANCE_ID` in `instances.go`).
4. **UI**: a Settings section (own page or a card, following `ApiKeySettings.tsx`/`SettingsPage.tsx` conventions) — enable toggle, image field, generated secrets shown masked (`has_override`-style badges as done for Brave), and per-instance "connected to OpenConnector: yes/no" indicator.
5. **Backups**: the connector's SQLite data volume needs to be added to the existing backup story (`docs/backups.md`) if the operator wants it covered — either fold into the same backup subsystem (new alias, e.g. `CONNECTOR`) or document that a Postgres-backed deployment should be backed up externally.

**Effort: Medium.** Rough breakdown (single engineer, familiar with the codebase):
| Piece | Effort |
|---|---|
| Settings CRUD + encrypted secrets + migrations (noop-style, matching `migration_00016/17` pattern) | 0.5–1 day |
| `connectorprov` package + `Apply()`-based lifecycle + health-check readiness | 1–1.5 days |
| Admin-API client + per-instance token minting + env injection + `EnsureEnvPropagated` wiring | 1 day |
| Settings UI + instance UI indicator | 1 day |
| Docs + backup story + tests | 0.5–1 day |
| **Total** | **~4–5.5 days** |

### Level 2 — Kubernetes backend parity
Because `Apply()` is already backend-agnostic, most of Level 1's `connectorprov` code is shared. Kubernetes-specific remainder:
- Decide PVC vs. external Postgres as the default for multi-replica control-plane deployments (SQLite + a single Pod is fine; a `ReadWriteOnce` PVC pins it to one node, matching how the control-plane's own Deployment is likely already pinned).
- Service + NetworkPolicy: reuse the existing `controlPlaneSelector` + per-workload NetworkPolicy pattern (`kubernetes_apply.go`) so only the control-plane (which relays instance calls) or agent pods directly can reach the connector's ClusterIP — decide which model matches how instances are expected to call it (see Open Question #1 below).
- Helm chart: new optional `connector.*` values block (image, resources, storage size, enabled flag), following `helm/values.yaml` conventions.

**Effort: +1–2 days** on top of Level 1, mostly Helm chart plumbing and picking the storage/networking model.

---

## 3. Recommendation

Ship **Level 0 now** (pure docs, ~1 hour) so Shaun/operators can wire this up manually today with the fork's `tip` image, using the existing per-instance/global env-var mechanism and the `open-connector` skill that's already installed in agent workspaces.

Scope **Level 1 (Docker backend)** as a proper follow-up ticket if/when there's a concrete need for Claworc to manage the connector's lifecycle itself (secret generation, per-instance token minting, UI visibility) rather than an operator doing it by hand. This is genuinely useful — automatic per-instance scoped tokens are a meaningfully better security story than one shared token pasted into every instance's env — but it's not required to get *a* working connector today.

Defer **Level 2 (Kubernetes)** until Level 1 is validated in the Docker backend; the `Apply()` abstraction means it should mostly fall out for free plus Helm values wiring.

---

## 4. Open questions for Shaun

1. **Network topology**: should every agent instance be able to dial the connector directly (agent container → connector container, both on the `claworc` bridge / same K8s namespace), or should agent instances always go through the control-plane as a relay (more auditable, matches the SSH-gateway-relay pattern used elsewhere, but adds a proxy hop)? This changes whether the connector needs a NetworkPolicy opened to agent pods or just to the control-plane.
2. **Multi-tenancy**: is a single shared open-connector deployment per Claworc control-plane sufficient (per-instance scoping done entirely via runtime-token policy), or does Shaun want per-team isolation (one connector deployment per `Team`, matching the existing `teams` model)? Single-shared is much cheaper to build and is what the current env vars in *this* container already imply.
3. **Which image reference is authoritative long-term**: `ghcr.io/shaunbarlow/open-connector:tip` (fork, bleeding edge, matches "run a fork" ask) vs. eventually pinning to release tags on the fork once it cuts its own releases? Recommend defaulting `connector_image` to the fork's `tip` for now (configurable setting either way) since that's explicitly what was asked for, with a documented recommendation to pin a release tag for anything resembling production.
4. **Backup coverage**: does the connector's credential/connection database need to be included in Claworc's existing backup subsystem, or is that out of scope (operator manages their own Postgres backups if they choose that path)?

---

## 5. Appendix — file/code pointers used for this scoping

- `control-plane/internal/orchestrator/orchestrator.go` — `ContainerOrchestrator` interface, `Apply`/`DeleteWorkload`/`WorkloadSSHAddress`.
- `control-plane/internal/orchestrator/spec.go` — `WorkloadSpec`, `VolumeMount`, `PortSpec`, etc.
- `control-plane/internal/orchestrator/docker_apply.go`, `kubernetes_apply.go` — backend implementations of `Apply()`.
- `control-plane/internal/browserprov/local.go` — closest existing precedent for "control-plane manages a second generic workload alongside an instance."
- `control-plane/internal/handlers/search.go`, `contextengine.go`, `settings.go` — precedent for optional-plugin-requiring-a-key settings, per-instance override, encrypted secret storage, live SSH config push + `EnsureEnvPropagated`.
- `control-plane/internal/handlers/instances.go` (~L778, ~L1305) — where computed env vars are injected into `CreateParams.EnvVars` at instance create/update; same call site for `OOMOL_CONNECT_RUNTIME_TOKEN`/`OPEN_CONNECTOR_BASE_URL`.
- `control-plane/internal/handlers/env_propagation.go` — `EnsureEnvPropagated`, restart-on-drift mechanism.
- `control-plane/docker-compose.yml` — confirmed "example only" comment; not used by `install.sh`.
- `projects/open-connector/docker-compose.yml`, `docker-compose.build.yml`, `docker/Dockerfile`, `docs/docker-ghcr.md` — authoritative env var / image / port / volume contract for the connector image.
- `projects/open-connector/.github/workflows/publish-docker.yml` — confirms `tip` = latest `active` commit, multi-arch, no fork-specific changes needed to keep using it.
- `~/.openclaw/workspace/skills/open-connector/SKILL.md` — the agent-facing consumption contract (base URL + scoped runtime token in env) that any provisioning path must satisfy.
- This container's own env (`OOMOL_CONNECT_RUNTIME_TOKEN`, `OOMOL_CONNECT_ADMIN_TOKEN` present) — live evidence a shared-connector model is already assumed somewhere in the current deployment, even though Claworc itself doesn't yet manage it.
