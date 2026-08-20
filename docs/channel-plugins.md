# Channel Plugins (Slack, Discord)

Every chat channel in OpenClaw is driven by a plugin. This documents how that
plugin comes to be running in an agent, and what Claworc does and does not do
about it.

## Slack and Discord are not part of OpenClaw

They used to be. Both shipped inside the openclaw npm package until they were
split into separate `@openclaw/<channel>` packages — Discord around host
`2026.4.10`, Slack around `2026.5.12-beta.1`, per the `minHostVersion` fields
in OpenClaw's own catalog.

The published tarball now **excludes them by name**. Its `files` list carries
`!dist/extensions/discord/**` and `!dist/extensions/slack/**` alongside ~70
other stripped extensions. They are not dependencies either — openclaw depends
only on `@openclaw/ai`, `@openclaw/fs-safe` and `@openclaw/proxyline` — and the
package's postinstall says so outright: *"Plugin package dependencies are
installed only by explicit plugin install/update flows, never postinstall."*

Verify against the artifact, never the repo. A local openclaw checkout can be
months behind the published package, and its `package.json` will still list
`extensions/` under `files`:

```bash
npm view openclaw@latest dist.tarball        # then download and inspect
tar tzf openclaw-*.tgz | grep 'dist/extensions/' | cut -d/ -f4 | sort -u
```

Plenty of channels *are* still bundled — `telegram`, `imessage`, `memory-core`,
`browser` and around 65 others live in `dist/extensions/`. The split is
partial, which is exactly what makes "it ships with OpenClaw" a plausible and
wrong assumption.

### How Claworc installs them

The agent image only runs `npm install -g openclaw`, so a fresh agent has
neither plugin. `EnsureChannelPluginInstalled` fills the gap: when a channel is
enabled — on a settings save or at create time — Claworc installs the package
on the agent over SSH and restarts the gateway.

Auto-enable cannot substitute for this. `applyPluginAutoEnable` only flips
`plugins.entries.<id>.enabled` for plugins OpenClaw *discovered*, and an
uninstalled plugin is never discovered.

The install is asynchronous and best-effort by design:

- It is an npm install inside the pod, needing registry egress, and can take
  minutes — so it must never sit on the request path.
- It runs only when the readback confirms the plugin is **missing**. An agent
  that could not be asked (`unknown`) is left alone: that is not evidence of
  absence, and acting on it would mean an npm install every time an agent was
  briefly unreachable. A `disabled` plugin is left alone too — that is a config
  decision reinstalling would not change.
- Duplicate installs for the same instance and channel are dropped, not
  queued.
- It waits up to three minutes for SSH, because the same save may have just
  triggered a container restart for a token change.

Outcome is reported by the readback on the next card load, not by the save.

## Enabling is automatic (once installed)

At **every gateway start**, OpenClaw calls `applyPluginAutoEnable`, which sets
`plugins.entries.<id>.enabled = true` for any *discovered* plugin whose channel
looks configured. For our two channels that means:

- **Discord** — `DISCORD_BOT_TOKEN` in the environment, *or* a non-empty
  `channels.discord` block.
- **Slack** — `SLACK_BOT_TOKEN` / `SLACK_APP_TOKEN` / `SLACK_USER_TOKEN`, *or*
  a non-empty `channels.slack` block.

Claworc writes both the token env var and the config block, so either alone
would be enough. This matters because bundled plugins are **disabled by
default** unless listed in OpenClaw's `BUNDLED_ENABLED_BY_DEFAULT`; the
auto-enable pass is what actually turns them on.

Both Claworc propagation paths end in a gateway start, which is what makes
this reliable rather than lucky:

- **Config-only edit** → `applyDiscordConfig`/`applySlackConfig` run
  `config set` then `gateway stop`; s6 respawns `svc-openclaw`.
- **Token change** → container restart → the boot script writes env + config →
  the gateway starts.

## What blocks it

`resolveEnableState` (OpenClaw's `src/plugins/config-state.ts`) refuses to
enable a plugin, in this order:

| Condition | Reported reason |
|---|---|
| `plugins.enabled: false` | `plugins disabled` |
| id in `plugins.deny` | `blocked by denylist` |
| `plugins.allow` non-empty and id **not** in it | `not in allowlist` |
| `plugins.entries.<id>.enabled: false` | `disabled in config` |
| bundled and not enabled-by-default | `bundled (disabled by default)` |

### The `plugins.allow` trap

`plugins.allow` is an **allowlist, not a hint**: when it is non-empty, *only*
the listed ids load — every other plugin, including other channels and
`memory-core`, is refused with `not in allowlist`.

Nothing in OpenClaw ever *creates* that array. All three writers —
`ensurePluginAllowlisted` (auto-enable), the copy in `plugins/enable.ts`, and
`addInstalledPluginToAllowlist` (the install flow) — append only when it
already exists, and the install-flow one additionally requires it to be
non-empty:

```ts
if (!Array.isArray(allow) || allow.includes(pluginId)) return cfg;
```

So an `allow` array only appears if a human, an agent, or a security-audit
remediation put it there — and once it exists, it silently gates everything
not yet in it. The failure is quiet and asymmetric: a channel configured
*before* the array appeared may already be inside it and keep working, while
a channel configured *after* is refused, with correct-looking config on disk.

Claworc deliberately does **not** add ids to `plugins.allow`. An allowlist is
a security control (OpenClaw's own audit recommends setting one), and quietly
widening it on a user's behalf would defeat the point. Claworc reports the
state instead — see below — and leaves the edit to the operator:

```
openclaw config get plugins            # inspect
openclaw plugins list --json           # per-plugin status + reason
openclaw plugins doctor                # load errors
```

## The readback

Enabling a channel used to be a write Claworc never confirmed: if the plugin
did not load, the settings card still showed green and the only symptom was a
bot that never answered — the same shape as the env-var bug in
`docs/env-propagation.md`.

`GET /api/v1/instances/{id}/{slack,discord}` now returns a `plugin_status`
object read off the agent via `openclaw plugins list --json`:

| `state` | Meaning |
|---|---|
| `loaded` | Running; the channel can work. |
| `disabled` | Present but not running. `detail` carries OpenClaw's reason from the table above — that string is the whole diagnostic. |
| `error` | Present but failed to load; `detail` carries the reason. |
| `missing` | Not installed on the agent. Enabling the channel triggers the background install described above. |
| `unknown` | The agent could not be asked. Says nothing about health. |

Plus `checking`, returned while the first probe is still running.

### Why it is cached rather than probed inline

The probe is slow, and badly underestimating it broke the whole feature once
already. `ExecOpenclaw` runs the command through `su - claworc` — a *login*
shell that sources a profile reaching into the Homebrew PVC — and the command
then boots Node and has OpenClaw discover and load every plugin it can find.
The original 8-second inline budget timed out every time, which was worse than
no feature at all: the card learned nothing, **and** the on-demand installer,
which asks the same question before acting, read the timeout as "cannot
confirm" and refused to install.

So the probe runs off the request path, on a 90s budget, and results are cached
for `pluginStatusCacheTTL`. A GET serves the last known answer and refreshes in
the background; the card shows `checking` only until the first probe lands, and
the frontend polls while it does.

Design constraints worth preserving:

- **Never probe inline.** Any budget short enough for a request is too short
  for the command.
- **A failed probe must not overwrite a good cached answer.** A brief restart
  would otherwise erase what we know and blank the card until the agent is
  back.
- **The installer probes synchronously on the full budget**, and never reads
  the cache — it can afford to wait, and acting on a `checking` placeholder is
  exactly the guess it exists to refuse.
- **GET only.** A token-change PUT restarts the container; asking a departing
  agent about its plugins would just burn the timeout.
- **Only when the channel is enabled.** An agent with the channel off has
  nothing to report, and the probe is not free.
- **`unknown` is neutral, never a warning.** "We could not check" and "it is
  broken" are different claims; conflating them trains people to ignore the
  warning that matters.

### Measuring it on a real agent

If the probe still times out, time the command itself before changing budgets:

```bash
time su - claworc -c 'openclaw plugins list --json > /dev/null'
```

Splitting that into the login-shell cost (`time su - claworc -c true`) and the
command cost tells you which half to attack.
