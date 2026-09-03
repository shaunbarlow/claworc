# Builtin Memory Configuration

Claworc configures agents' OpenClaw memory subsystem via OpenClaw's
**builtin** memory engine only. The QMD sidecar backend (local-first hybrid
search, `github.com/tobi/qmd`) has been retired end-to-end — see "History"
below. There is no backend selector anymore: every non-legacy agent uses
OpenClaw's stock SQLite index, and Claworc's job is just curating
`memory.search.*` (embedding provider, query limits, session-transcript
indexing) and top-level `memory.citations`.

Reference: <https://docs.openclaw.ai/concepts/memory-builtin>,
<https://docs.openclaw.ai/reference/memory-config>.

## Resolution model

Every memory-affecting value resolves in three layers:

1. **Per-instance override** — `Instance.MemorySettings` (JSON
   `MemorySettings`, field-wise override; `""`/unset fields inherit).
2. **Global defaults** — setting key `default_memory_settings` (JSON, seeded
   `{}`).
3. **OpenClaw defaults** — anything still unset is simply omitted from the
   pushed config, so OpenClaw's own defaults apply (e.g. auto-detecting
   OpenAI embeddings when `OPENAI_API_KEY` is configured).

`MemorySettings` (Go: `internal/handlers/memory.go`, TS:
`common/types/instance.ts`):

| Field | OpenClaw config path | Notes |
|---|---|---|
| `provider` | `memory.search.provider` | Embedding adapter id (`openai`, `gemini`, `bedrock`, `deepinfra`, `mistral`, `voyage`, `ollama`, `lmstudio`, `local`, `openai-compatible`), `"none"` for deliberate FTS-only, or a custom `models.providers.<id>` key |
| `model` | `memory.search.model` | Embedding model name override |
| `fallback` | `memory.search.fallback` | Adapter id tried when the primary provider fails |
| `max_results` | `memory.search.query.maxResults` | 1–100 (OpenClaw default 6) |
| `min_score` | `memory.search.query.minScore` | 0.0–1.0 |
| `citations` | top-level `memory.citations` | `auto` (default) / `on` / `off` |
| `remember_across_conversations` | `memory.search.rememberAcrossConversations` | Let this agent recall context from its own other recognized private conversations |
| `sessions_enabled` | `memory.search.sources` | Adds/removes `"sessions"` alongside the always-present `"memory"` source |
| `advanced` | deep-merged into `memory.search` last | Escape hatch: multimodal, remote endpoint/headers, `store.vector`, `cache`, input-type labels, ... |

`sessions_enabled` and `remember_across_conversations` are independent:
`remember_across_conversations: true` implies session indexing on OpenClaw's
side regardless of `sessions_enabled`, but `sessions_enabled` alone just
makes past sessions searchable via `memory_search` without the broader
cross-conversation recall grant.

## Shared folders in the index

A shared folder can be flagged `memory_index` (+ optional
`memory_index_pattern` glob narrowing it; empty indexes the whole folder).
Every attached agent (explicit or team-implicit) gets the folder rendered as
a `memory.search.extraPaths` entry — a bare path string when unpatterned, or
`{path, pattern}` when a pattern is set. Read-only folders work fine; the
builtin engine's SQLite index lives under
`~/.openclaw/agents/<id>/agent/openclaw-agent.sqlite` on the home PVC, never
inside the folder.

## Config propagation

`buildMemoryConfig` resolves the full `memory` subtree (`citations` +
`search`); `applyMemoryConfig` replaces it wholesale in one write (`openclaw
config set memory <json> --replace --json`, so removed folders and cleared
overrides disappear; never `config unset` first — that write is rejected by
OpenClaw's size-drop guard, and when it does land ahead of a failing set the
agent loses its memory config entirely) and restarts the gateway (`openclaw
gateway stop --force`). Top-level `memory.*` has no dedicated hot-reload rule in
OpenClaw's config-reload planner (confirmed against the shipped
`config-reload-plan` rule table), so it falls through to the default
"restart" classification — a full Gateway restart is required either way.
**Claworc owns the `memory.*` subtree** — manual edits to it via the raw
Config tab are overwritten on the next push.

Pushes are async and best-effort (`pushMemoryConfig`, 120s SSH wait to ride
out container restarts; `openclaw.json` lives on the home PVC so a push that
lands just before a restart still survives it). Triggers:

- instance create (seeded after `ConfigureInstance`)
- `PATCH /api/v1/instances/{id}/memory`
- global `default_memory_settings` change → every running non-legacy
  instance (gateway restart only, not a container restart)
- shared-folder memory-index flag/pattern change, and membership/mount-path
  changes of an indexed folder → affected instances (old ∪ new effective
  set)
- shared-folder delete of an indexed folder → previously covered instances

Legacy embedded instances are excluded everywhere.

## API

- `GET /api/v1/instances/{id}/memory` → override + effective values +
  `indexed_folders`
- `PATCH /api/v1/instances/{id}/memory` body `{settings: MemorySettings}`
  (full replacement of the per-instance override)
- `GET/PUT /api/v1/settings` — `default_memory_settings`
- Shared folder create/update accept `memory_index` / `memory_index_pattern`

## UI

- **Settings → Environment → Memory Defaults**: global embedding
  provider/knobs + advanced JSON (self-contained card,
  `MemorySettingsEditor`).
- **Agent → Settings → Memory**: per-agent override with inherit
  placeholders, effective values, and the list of indexed shared folders.
- **Shared Folders**: "Include in memory index" checkbox + file pattern.

## History: QMD retirement

Claworc previously also supported switching an agent's memory backend to
[QMD](https://github.com/tobi/qmd) (a local-first hybrid search sidecar:
BM25 + vector embeddings + reranking, baked into the agent image via Bun).
QMD has been fully removed:

- the `memory_backend` selector, `MemoryQmdSettings`/`memory.qmd.*`
  rendering, and the `qmd`/`vsearch`/`query` search-mode knobs are gone
- `Instance.MemoryBackend` / `Instance.MemoryQmd` and
  `SharedFolder.QmdIndex` / `SharedFolder.QmdPattern` columns were dropped
  (migration `00019_retire_qmd_memory`)
- the `bun` runtime and `@tobilu/qmd` package are no longer baked into
  `agent/instance/Dockerfile`
- `agent/tests/qmd.test.ts` (binary-on-PATH checks) was removed

There was no builtin-compatible carryover for QMD-specific values
(`search_mode`, `update_interval`, scope rules) — any agent that was
actually running on the qmd backend falls back to builtin on its next
config push, matching OpenClaw's own upstream QMD-removal behavior (agents
keep working; they just lose the QMD-specific index and rebuild via
builtin's own indexing on the next sync).
