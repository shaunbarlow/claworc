# Channel Plugins (Slack, Discord)

Every chat channel in OpenClaw is driven by a plugin. This documents how that
plugin comes to be running in an agent, and what Claworc does and does not do
about it.

## Nothing needs installing

`extensions/slack` and `extensions/discord` ship **inside** the OpenClaw npm
package — its `package.json` `files` list includes `extensions/`. The agent
image installs OpenClaw globally (`agent/instance/Dockerfile`), so both land at
`$(npm root -g)/openclaw/extensions/`, and `resolveBundledPluginsDir()` finds
them by walking up from its own module. They are discovered with
`origin: "bundled"`.

There is no `openclaw plugins install` step, and Claworc does not perform one.

## Enabling is automatic

At **every gateway start**, OpenClaw calls `applyPluginAutoEnable`, which sets
`plugins.entries.<id>.enabled = true` for any plugin whose channel looks
configured. For our two channels that means:

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

Nothing in OpenClaw ever *creates* that array. Both writers
(`ensureAllowlisted` in `plugin-auto-enable.ts` and in `plugins/enable.ts`)
append only when it already exists:

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
| `missing` | Not in the agent at all. Points at the agent image, not at these settings. |
| `unknown` | The agent could not be asked. Says nothing about health. |

Design constraints worth preserving:

- **GET only.** A token-change PUT restarts the container; asking a departing
  agent about its plugins would just burn the timeout.
- **Only when the channel is enabled.** An agent with the channel off has
  nothing to report, and the readback costs an SSH round-trip.
- **`GetConnection`, not `WaitForSSH`.** This runs inline in a request and
  must never block waiting for an agent to come up. No live connection means
  `unknown`, immediately.
- **`unknown` is neutral, never a warning.** "We could not check" and "it is
  broken" are different claims; conflating them trains people to ignore the
  warning that matters.
