# Per-Agent Discord Connections

Claworc can connect each agent (OpenClaw instance) to Discord with a
structured per-agent config: a bot token plus a server/channel allowlist and
DM policy. It is the Discord counterpart of the Slack feature (see
`docs/slack-connections.md`) and shares its architecture; this doc covers the
Discord-specific parts.

## Prerequisites (Discord side)

Claworc cannot create the Discord app itself. You need:

- A Discord application with a **bot** and its **bot token** (Developer
  Portal → Bot → Reset Token). One token is sufficient — OpenClaw connects
  via the Gateway/WebSocket transport only, so there is no app-level token,
  public key, or interactions endpoint to configure.
- **Message Content Intent enabled** in Developer Portal → Bot → Privileged
  Gateway Intents. Without it the gateway connection fails with a
  disallowed-intents error.
- The bot invited to each server with scopes `bot` + `applications.commands`
  and baseline permissions (View Channels, Send Messages, Read Message
  History, Embed Links, Attach Files).

## Differences from Slack

- **Single credential**: the token is stored in the instance's encrypted
  env-vars map as `DISCORD_BOT_TOKEN` (OpenClaw's env fallback for the
  default account when `channels.discord.token` is unset).
- **Two-level allowlist**: entries are `{guild_id, channel_id?}` pairs keyed
  by raw numeric snowflake IDs (Discord → Developer Mode → right-click → Copy
  ID). An entry with no channel ID allows the *whole server*; a server with
  channel entries restricts to exactly those channels. Mixing both styles for
  the same server is rejected. Rendered shape:

  ```json
  {
    "enabled": true,
    "groupPolicy": "allowlist",
    "guilds": {
      "123456789012345678": {
        "channels": {
          "234567890123456789": { "allow": true, "requireMention": true }
        }
      },
      "999999999999999999": { "requireMention": true }
    },
    "dmPolicy": "pairing"
  }
  ```

- **`groupPolicy` is always written explicitly**: unlike Slack (which
  defaults to allowlist), OpenClaw's Discord runtime defaults to
  `groupPolicy: "open"` when unset — every guild the bot is in would be
  answered. Claworc always renders `"allowlist"` for secure-by-default
  behavior.

## DM policy

`dm_policy` maps onto OpenClaw's `channels.discord.dmPolicy`:

| `dm_policy` | Behavior |
|---|---|
| `""` / `"pairing"` | Default. An unknown user must complete a one-time pairing approval before the agent answers. |
| `"allowlist"` | Only the users in `dm_allow_from` are answered, **with no pairing handshake**. Everyone else is ignored. |
| `"open"` | Anyone who can DM the bot gets a response. |
| `"disabled"` | DMs are ignored entirely. |

`allowlist` is the option for "these specific people, no ceremony". The user
IDs are rendered into `allowFrom`, the same field `open` fills with `"*"`:

```json
{ "dmPolicy": "allowlist", "allowFrom": ["111111111111111111"] }
```

IDs are raw numeric snowflakes (Developer Mode → right-click a user → Copy
ID). A pasted mention (`<@123…>`, `<@!123…>`) is normalized down to the bare
ID; a username is rejected, because it would silently fail to match. An empty
`dm_allow_from` under `allowlist` is rejected too — it blocks every DM while
reading as "some users are allowed", which is `disabled` under the wrong name.
The list is preserved when you switch to another policy, so toggling in the UI
does not discard it.

Everything else matches Slack: structure in `Instance.DiscordConfig`
(additive column, noop migration 00014), boot-time delivery via the reserved
`OPENCLAW_INITIAL_DISCORD` env var applied by `svc-openclaw/run`
(unset-then-set, authoritative when present, hands off when absent), live SSH
push + gateway restart for config-only edits, container restart for token
changes, `GET/PUT /api/v1/instances/{id}/discord`, a `discord` object on
instance create, the settings-tab card, and the create-form section. Clones
carry no Discord connection.

The token restart is not decided by "did the row change" — it is decided by
`EnsureEnvPropagated`, which compares the live container's environment against
what the database says it should be. See `docs/env-propagation.md`.

## Scope / deferred

Default Discord account only. Multi-account (`channels.discord.accounts.*`),
per-guild `users`/`roles` sender allowlists, role-based agent bindings
(`bindings[].match.roles`), group-DM settings, and the presence/guild-members
privileged intents are not surfaced — all remain reachable by editing the
agent's OpenClaw config directly, as long as Discord is not also managed
through this feature (Claworc owns the whole `channels.discord` path once
used).
