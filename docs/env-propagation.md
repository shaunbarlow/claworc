# Environment Variable Propagation

Env vars reach an agent container exactly once: when its spec is built. On
Kubernetes that is the Deployment's pod template; on Docker it is the
container's `Config.Env`. Nothing re-reads them afterwards, and there is no
way to inject one into a container that is already running.

That single fact drives everything in this document. A value written to the
database is *not* in the agent until some code path rebuilds the spec.

## Why "restart when the row changes" was wrong

Every path that writes an env var used to decide for itself whether to
restart, by inferring from a change event: *the row changed, so restart*. That
inference fails in both directions.

**It fires when it shouldn't reach anything.** The `instances.status` column
lags reality. It sits at `creating` for the whole provisioning window, and
`enrichStatus` only ever writes a status back when leaving `restarting` — a
row left at `stopped` or `error` in front of a healthy pod stays that way
indefinitely. A `status == "running"` gate therefore drops the restart
silently, and the handler still returns 200.

**It doesn't fire when it should.** `ApplyEnvVarsDelta` reports `changed` by
comparing the new plaintext against the *stored* plaintext — database to
database, never database to container. So once a write has landed, saving the
same value again reports no change and skips the restart. A value that missed
its window the first time can never be pushed again; only a manual restart
recovers it.

Both failures compose into a sticky state that is easy to hit and hard to
read: an agent created with a channel token, where the token save raced the
container spin-up. The UI shows the token set, the config pushes fine over
SSH, and the container never has the variable.

## The mechanism

`EnsureEnvPropagated` (`internal/handlers/env_propagation.go`) replaces the
inference with a fact. It reads the env the container is *actually* running
with via `ContainerOrchestrator.GetInstanceEnv`, compares it against what
`buildCreateParams` would inject today, and restarts only on a real
difference.

Because the decision is made against live state rather than an event, it is
idempotent, safe to call from any path, and self-healing: a propagation that
was missed is simply drift on the next call.

```go
if EnsureEnvPropagated(ctx, inst, userID, "DISCORD_BOT_TOKEN") {
    // a restart was started
}
```

### What counts as drift

- **Every desired var must be present and equal in the container.** Vars only
  in the container are ignored — the image contributes env Claworc does not
  own, and treating that as drift would restart everything on every save.
- **Removals must be declared.** A var the caller deleted is gone from the
  desired map, so the subset check cannot see that the container still has it.
  Callers pass the names they touched; only they know which keys were theirs
  as opposed to the image's.
- **`OPENCLAW_INITIAL_*` is exempt.** Those vars only seed the agent's OpenClaw
  config at boot and each has a live push path over SSH (`pushSlackConfig`,
  `pushDiscordConfig`, `ConfigureInstance`). A stale copy in a running
  container is not worth a restart — the agent's config is already correct and
  the var is rewritten at the next boot. Counting them as drift would turn
  every channel-config edit into a container restart instead of a live update.

### Liveness

`EnsureEnvPropagated` gates on the **live** orchestrator status, never the
status column, and leaves a non-running instance alone: its next create or
start builds a fresh spec straight from the database, so there is nothing to
reconcile. `restartInstanceAsyncWithToast` applies the same live gate, which
is what makes it safe to call from a path that does not know the column is
accurate — the same reasoning the tunnel reconciler uses when it deliberately
accepts `restarting`/`error` rows (see `StartBackgroundManager` in `main.go`).

## Where the spec gets rebuilt

`buildCreateParams` is the single source of truth for `CreateParams`. Every
path that builds a spec goes through it:

| Path | Behavior |
|---|---|
| `CreateInstance` | Re-reads the row inside the provisioning task before building the spec — the closure captured it before an image pull, and anything written in that window would otherwise be lost. Runs a drift check once SSH is up, to catch writes that landed after the spec was built. |
| `RestartInstance` (manual) | Rebuilds from the database. |
| `StartInstance` | Rebuilds from the database rather than scaling the existing workload back up — a bare start would resurrect the container with whatever env it had when it was stopped. |
| `UpdateImage` | Rebuilds the whole pod spec, so env rides along. |
| `UpdateInstance`, Slack, Discord, global settings | Write, then call `EnsureEnvPropagated`. |

`OPENCLAW_INITIAL_MODELS` / `OPENCLAW_INITIAL_PROVIDERS` are the exception:
they are resolved in `CreateInstance` because they depend on the LLM gateway
keys minted there, and are applied on top of `buildCreateParams`. A restart
does not currently re-derive them, which is safe only because
`ConfigureInstance` re-applies models and providers over SSH.

## Adding a new env var

1. Make sure it ends up in `buildCreateParams`, directly or through the
   instance's encrypted `EnvVars` map.
2. After writing it, call `EnsureEnvPropagated` with its name in `touched`.
3. Do not gate that call on `inst.Status`, and do not gate it on whether your
   write reported a change. Both are the bugs described above.
