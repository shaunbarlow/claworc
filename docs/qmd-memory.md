# QMD Memory Backend

Claworc can switch an agent's OpenClaw memory subsystem from the builtin
SQLite index to [QMD](https://github.com/tobi/qmd), a local-first hybrid
search sidecar (BM25 + vector embeddings + reranking, fully offline after
the first model download). Requires OpenClaw >= 2026.2.2; the `qmd` binary
is baked into the agent image (`agent/instance/Dockerfile`, symlinked to
`/usr/local/bin/qmd` so it is on the gateway process's PATH).

## Resolution model

Every memory-affecting value resolves in three layers:

1. **Per-instance override** — `Instance.MemoryBackend` (`""` = inherit) and
   `Instance.MemoryQmd` (JSON `MemoryQmdSettings`, field-wise override).
2. **Global defaults** — settings keys `default_memory_backend`
   (`builtin`, seeded) and `default_memory_qmd` (JSON, seeded `{}`).
3. **OpenClaw defaults** — anything still unset is simply omitted from the
   pushed config, so OpenClaw's own defaults apply.

`MemoryQmdSettings` (Go: `internal/handlers/memory.go`, TS:
`common/types/instance.ts`):

| Field | OpenClaw config path | Notes |
|---|---|---|
| `search_mode` | `memory.qmd.searchMode` | `search` (default) / `vsearch` / `query`; `query` is slow on CPU-only hosts |
| `update_interval` | `memory.qmd.update.interval` | e.g. `5m`, `1h` |
| `max_results` | `memory.qmd.limits.maxResults` | 1–50 |
| `sessions_enabled` | `memory.qmd.sessions.enabled` | index session transcripts |
| `include_default_memory` | `memory.qmd.includeDefaultMemory` | workspace `MEMORY.md` / `memory/**/*.md` |
| `advanced` | deep-merged into `memory.qmd` last | escape hatch for scope rules, timeouts, debounce, ... |

## Shared folders in the index

A shared folder can be flagged `qmd_index` (+ optional `qmd_pattern` glob,
default `**/*.md`). Every attached agent (explicit or team-implicit) whose
effective backend is `qmd` gets the folder's mount path pushed as a
`memory.qmd.paths` entry, named `<slug>-<folderID>`. Read-only folders work
fine — QMD keeps its index under `~/.openclaw/agents/<id>/qmd/` on the home
PVC, never inside the folder.

## Config propagation

`buildMemoryConfig` resolves the full `memory` subtree; `applyMemoryConfig`
replaces it wholesale in one write (`openclaw config set memory <json>
--replace --json`, so removed folders and cleared overrides disappear; never
`config unset` first — that write is rejected by OpenClaw's size-drop guard,
and when it does land ahead of a failing set the agent loses its memory config
entirely) and restarts the gateway (`openclaw gateway stop`; memory.* is not
hot-reloaded by OpenClaw). **Claworc owns the `memory.*` subtree** — manual
edits to it via the raw Config tab are overwritten on the next push.

Pushes are async and best-effort (`pushMemoryConfig`, 120s SSH wait to ride
out container restarts; `openclaw.json` lives on the home PVC so a push that
lands just before a restart still survives it). Triggers:

- instance create (seeded after `ConfigureInstance`)
- `PATCH /api/v1/instances/{id}/memory`
- global `default_memory_backend` / `default_memory_qmd` change → every
  running non-legacy instance (gateway restart only, not a container restart)
- shared-folder QMD flag/pattern change, and membership/mount-path changes of
  an indexed folder → affected instances (old ∪ new effective set)
- shared-folder delete of an indexed folder → previously covered instances

Legacy embedded instances are excluded everywhere.

## API

- `GET /api/v1/instances/{id}/memory` → override + effective values +
  `indexed_folders`
- `PATCH /api/v1/instances/{id}/memory` body
  `{memory_backend?: ""|"builtin"|"qmd", qmd?: MemoryQmdSettings}` (the `qmd`
  object is a full replacement of the override)
- `GET/PUT /api/v1/settings` — `default_memory_backend`, `default_memory_qmd`
- Shared folder create/update accept `qmd_index` / `qmd_pattern`

## UI

- **Settings → Environment → Memory Defaults**: global backend + QMD knobs +
  advanced JSON (self-contained card, `MemorySettingsEditor`).
- **Agent → Settings → Memory**: per-agent override with inherit options,
  effective values as placeholders, and the list of indexed shared folders.
- **Shared Folders**: "Include in memory index" checkbox + file pattern.

## Runtime notes

- If the `qmd` binary is missing or broken, OpenClaw logs and falls back to
  the builtin backend — agents keep working.
- QMD downloads GGUF models from HuggingFace on first use (needs network
  once); models cache under `~/.cache/qmd/models` on the home PVC.
- `query` search mode runs a local reranker — noticeably heavier on CPU-only
  nodes; consider the agent's CPU limits before enabling it.
