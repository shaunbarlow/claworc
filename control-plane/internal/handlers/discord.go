package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/sshproxy"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

// The Discord bot token is stored in the instance's encrypted EnvVars map
// under this name. OpenClaw reads it from the environment for the default
// Discord account whenever channels.discord.token is unset, so the rendered
// config block never contains the token. Discord needs no app-level token —
// OpenClaw only connects via the Gateway/WebSocket transport.
const discordBotTokenEnvVar = "DISCORD_BOT_TOKEN"

// discordSnowflakeRegex matches raw Discord guild/channel IDs (numeric
// snowflakes). Names silently fail to route, so reject them up front.
var discordSnowflakeRegex = regexp.MustCompile(`^[0-9]{15,22}$`)

// discordChannelRule is one allowlist entry in the per-instance config: a
// guild plus optionally one of its channels. An entry with no channel ID
// allows the whole guild.
type discordChannelRule struct {
	GuildID string `json:"guild_id"`
	// ChannelID empty means the entire guild is allowed.
	ChannelID string `json:"channel_id,omitempty"`
	// RequireMention nil means OpenClaw's default (true): the bot only
	// responds when @-mentioned.
	RequireMention *bool `json:"require_mention,omitempty"`
}

// instanceDiscordConfig is the JSON shape persisted in Instance.DiscordConfig.
type instanceDiscordConfig struct {
	Enabled  bool                 `json:"enabled"`
	Channels []discordChannelRule `json:"channels"`
	// DMPolicy: "" (OpenClaw default, pairing), "pairing", "allowlist",
	// "open", "disabled".
	DMPolicy string `json:"dm_policy,omitempty"`
	// DMAllowFrom is the set of Discord user IDs allowed to DM the agent
	// under the "allowlist" policy: those users are let straight through with
	// no pairing handshake, and everyone else is blocked. Ignored for every
	// other policy.
	DMAllowFrom []string `json:"dm_allow_from,omitempty"`
	// AllowBots: "" (OpenClaw default, false -- bot-authored messages are
	// ignored), "true" (bot messages trigger replies same as humans), or
	// "mentions" (bot messages only trigger replies when they @-mention the
	// bot). Maps onto OpenClaw's channels.discord.allowBots.
	AllowBots string `json:"allow_bots,omitempty"`
}

func parseDiscordConfig(raw string) (instanceDiscordConfig, bool) {
	var cfg instanceDiscordConfig
	if raw == "" {
		return cfg, false
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return instanceDiscordConfig{}, false
	}
	if cfg.Channels == nil {
		cfg.Channels = []discordChannelRule{}
	}
	return cfg, true
}

// validateDiscordConfig normalizes IDs (trim, strip a leading '#') and
// rejects anything that isn't a numeric snowflake. Mixing a whole-guild
// entry with specific-channel entries for the same guild is rejected because
// the rendered config could only express one of the two.
func validateDiscordConfig(cfg *instanceDiscordConfig) error {
	type guildState struct{ whole, channels bool }
	guilds := map[string]*guildState{}
	seen := map[string]bool{}
	normalized := make([]discordChannelRule, 0, len(cfg.Channels))
	for _, rule := range cfg.Channels {
		gid := strings.TrimSpace(rule.GuildID)
		if !discordSnowflakeRegex.MatchString(gid) {
			return fmt.Errorf("invalid Discord server ID %q: use the numeric server (guild) ID, not its name", rule.GuildID)
		}
		cid := strings.TrimPrefix(strings.TrimSpace(rule.ChannelID), "#")
		if cid != "" && !discordSnowflakeRegex.MatchString(cid) {
			return fmt.Errorf("invalid Discord channel ID %q: use the numeric channel ID, not the channel name", rule.ChannelID)
		}
		st := guilds[gid]
		if st == nil {
			st = &guildState{}
			guilds[gid] = st
		}
		if cid == "" {
			st.whole = true
		} else {
			st.channels = true
		}
		if st.whole && st.channels {
			return fmt.Errorf("server %s mixes a whole-server entry with specific channels: keep either the whole server or a channel list", gid)
		}
		key := gid + "/" + cid
		if seen[key] {
			continue
		}
		seen[key] = true
		rule.GuildID = gid
		rule.ChannelID = cid
		normalized = append(normalized, rule)
	}
	cfg.Channels = normalized

	// Normalize the DM allowlist regardless of policy so switching to
	// "allowlist" and back does not silently corrupt a saved list. Users are
	// commonly pasted as a mention (<@123>) rather than a bare ID.
	allowFrom := make([]string, 0, len(cfg.DMAllowFrom))
	seenUser := map[string]bool{}
	for _, raw := range cfg.DMAllowFrom {
		uid := strings.TrimSpace(raw)
		uid = strings.TrimSuffix(strings.TrimPrefix(uid, "<@"), ">")
		uid = strings.TrimPrefix(uid, "!") // <@!123> is the legacy nickname form
		if uid == "" {
			continue
		}
		if !discordSnowflakeRegex.MatchString(uid) {
			return fmt.Errorf("invalid Discord user ID %q: use the numeric user ID, not the username", raw)
		}
		if seenUser[uid] {
			continue
		}
		seenUser[uid] = true
		allowFrom = append(allowFrom, uid)
	}
	cfg.DMAllowFrom = allowFrom

	switch cfg.DMPolicy {
	case "", "pairing", "open", "disabled":
	case "allowlist":
		// An empty allowlist would block every DM while reading in the UI as
		// "specific users are allowed" -- that is "disabled" wearing the wrong
		// label, so make the user say which one they meant.
		if len(cfg.DMAllowFrom) == 0 {
			return fmt.Errorf("dm_policy \"allowlist\" needs at least one Discord user ID (use \"disabled\" to block all DMs)")
		}
	default:
		return fmt.Errorf("invalid dm_policy %q: must be one of pairing, allowlist, open, disabled", cfg.DMPolicy)
	}

	switch cfg.AllowBots {
	case "", "true", "mentions":
	default:
		return fmt.Errorf("invalid allow_bots %q: must be one of \"\" (false), \"true\", \"mentions\"", cfg.AllowBots)
	}
	return nil
}

// renderDiscordChannelsJSON renders the stored config into the OpenClaw
// `channels.discord` block. The token is deliberately omitted — OpenClaw
// falls back to DISCORD_BOT_TOKEN from the environment for the default
// account. groupPolicy is always written explicitly: unlike Slack, OpenClaw's
// Discord runtime defaults to "open" when unset.
func renderDiscordChannelsJSON(cfg instanceDiscordConfig) (string, error) {
	block := map[string]interface{}{"enabled": cfg.Enabled}
	if cfg.Enabled {
		block["groupPolicy"] = "allowlist"
		// AllowBots: OpenClaw's own default is false when the key is unset, so
		// only write it for a non-default choice. "true" renders the boolean;
		// "mentions" passes through as OpenClaw's string variant (bot messages
		// only trigger a reply when they @-mention the bot).
		switch cfg.AllowBots {
		case "true":
			block["allowBots"] = true
		case "mentions":
			block["allowBots"] = "mentions"
		}
		if len(cfg.Channels) > 0 {
			guilds := map[string]map[string]interface{}{}
			for _, rule := range cfg.Channels {
				guild := guilds[rule.GuildID]
				if guild == nil {
					guild = map[string]interface{}{}
					guilds[rule.GuildID] = guild
				}
				requireMention := rule.RequireMention == nil || *rule.RequireMention
				if rule.ChannelID == "" {
					// Whole guild: mention gating lives at the guild level.
					guild["requireMention"] = requireMention
					continue
				}
				channels, _ := guild["channels"].(map[string]interface{})
				if channels == nil {
					channels = map[string]interface{}{}
					guild["channels"] = channels
				}
				// "enabled", not "allow". The old bundled Discord schema took
				// `allow`; the @openclaw/discord package's guild channel entry
				// is strict (additionalProperties: false) and takes `enabled`,
				// matching Slack. Sending `allow` fails validation for the
				// entire config, not just this key, so the agent comes up with
				// no Discord at all.
				channels[rule.ChannelID] = map[string]interface{}{
					"enabled":        true,
					"requireMention": requireMention,
				}
			}
			block["guilds"] = guilds
		}
		switch cfg.DMPolicy {
		case "open":
			// OpenClaw requires an explicit wildcard sender allowlist for open DMs.
			block["dmPolicy"] = "open"
			block["allowFrom"] = []string{"*"}
		case "allowlist":
			// Named users are let through with no pairing handshake; everyone
			// else is blocked. allowFrom carries the senders for this policy
			// exactly as the "*" wildcard does for "open".
			block["dmPolicy"] = "allowlist"
			block["allowFrom"] = cfg.DMAllowFrom
		case "pairing", "disabled":
			block["dmPolicy"] = cfg.DMPolicy
		}
	}
	b, err := json.Marshal(block)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// renderInitialDiscordEnv returns the OPENCLAW_INITIAL_DISCORD value for an
// instance, or "" when Discord has never been configured through Claworc (so
// a manually-edited channels.discord block in the agent's config is left
// alone).
func renderInitialDiscordEnv(inst database.Instance) string {
	cfg, ok := parseDiscordConfig(inst.DiscordConfig)
	if !ok {
		return ""
	}
	rendered, err := renderDiscordChannelsJSON(cfg)
	if err != nil {
		log.Printf("Failed to render Discord config for instance %d: %v", inst.ID, err)
		return ""
	}
	return rendered
}

// applyDiscordConfig writes the channels.discord block into the agent's
// OpenClaw config over an established SSH connection and restarts the gateway
// so it takes effect. The path is cleared first because `config set`
// deep-merges map values — guilds/channels removed in Claworc would otherwise
// linger.
func applyDiscordConfig(ctx context.Context, agent sshproxy.Instance, name, channelsJSON string) {
	_, _, _, _ = agent.ExecOpenclaw(ctx, "config", "unset", "channels.discord")
	_, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "set", "channels.discord", channelsJSON, "--json")
	if err != nil {
		log.Printf("Error setting channels.discord for %s: %v", utils.SanitizeForLog(name), err)
		return
	}
	if code != 0 {
		log.Printf("Failed to set channels.discord for %s: %s", utils.SanitizeForLog(name), utils.SanitizeForLog(stderr))
		return
	}
	if _, _, _, err := agent.ExecOpenclaw(ctx, "gateway", "stop"); err != nil {
		log.Printf("Error restarting gateway for %s after Discord config change: %v", utils.SanitizeForLog(name), err)
	}
}

// pushDiscordConfig is the async best-effort wrapper around
// applyDiscordConfig for a running instance (mirrors pushSlackConfig). A
// stopped or unreachable instance picks the config up at next boot via
// OPENCLAW_INITIAL_DISCORD.
func pushDiscordConfig(instanceID uint, name, channelsJSON string) {
	if SSHMgr == nil || channelsJSON == "" {
		return
	}
	go func() {
		ctx := context.Background()
		sshClient, err := SSHMgr.WaitForSSH(ctx, instanceID, 30*time.Second)
		if err != nil {
			log.Printf("discord-config: no SSH connection for instance %d, skipping OpenClaw config push: %v", instanceID, err)
			return
		}
		applyDiscordConfig(ctx, sshproxy.NewSSHInstance(sshClient), name, channelsJSON)
	}()
}

// instanceDiscordCreateRequest is the create-time Discord payload (structured
// config plus the bot token) accepted by CreateInstance, so a new agent
// connects to Discord on first boot.
type instanceDiscordCreateRequest struct {
	instanceDiscordConfig
	BotToken string `json:"bot_token"`
}

type instanceDiscordResponse struct {
	Configured     bool                 `json:"configured"`
	Enabled        bool                 `json:"enabled"`
	Channels       []discordChannelRule `json:"channels"`
	DMPolicy       string               `json:"dm_policy"`
	DMAllowFrom    []string             `json:"dm_allow_from"`
	AllowBots      string               `json:"allow_bots"`
	HasBotToken    bool                 `json:"has_bot_token"`
	BotTokenMasked string               `json:"bot_token_masked,omitempty"`
	Restarting     bool                 `json:"restarting,omitempty"`
	// PluginStatus is filled in by GET only -- see GetInstanceDiscord.
	PluginStatus *channelPluginStatus `json:"plugin_status,omitempty"`
}

func discordResponseFor(inst database.Instance) instanceDiscordResponse {
	cfg, configured := parseDiscordConfig(inst.DiscordConfig)
	resp := instanceDiscordResponse{
		Configured:  configured,
		Enabled:     cfg.Enabled,
		Channels:    cfg.Channels,
		DMPolicy:    cfg.DMPolicy,
		DMAllowFrom: cfg.DMAllowFrom,
		AllowBots:   cfg.AllowBots,
	}
	if resp.Channels == nil {
		resp.Channels = []discordChannelRule{}
	}
	if resp.DMAllowFrom == nil {
		resp.DMAllowFrom = []string{}
	}
	// Token presence reflects what the container actually receives: global
	// defaults merged with per-instance overrides.
	envVars := map[string]string{}
	MergeUserEnvVars(envVars, LoadGlobalEnvVars(), LoadInstanceEnvVars(inst))
	if v, ok := envVars[discordBotTokenEnvVar]; ok && v != "" {
		resp.HasBotToken = true
		resp.BotTokenMasked = utils.Mask(v)
	}
	return resp
}

// GET /api/v1/instances/{id}/discord
//
// Reads the plugin status back off the agent, but only when Discord is
// actually meant to be running: an agent with Discord off has nothing to
// report, and the readback costs an SSH round-trip. It is attached here
// rather than in discordResponseFor so the PUT path stays fast -- a token
// change restarts the container, and asking a restarting agent about its
// plugins would just burn the timeout.
func GetInstanceDiscord(w http.ResponseWriter, r *http.Request) {
	inst, ok := resolveInstanceForChannelSettings(w, r)
	if !ok {
		return
	}
	resp := discordResponseFor(*inst)
	if resp.Configured && resp.Enabled {
		status := channelPluginStatusCached(inst.ID, "discord")
		resp.PluginStatus = &status
	}
	writeJSON(w, http.StatusOK, resp)
}

type instanceDiscordUpdateRequest struct {
	Enabled     *bool                 `json:"enabled"`
	Channels    *[]discordChannelRule `json:"channels"`
	DMPolicy    *string               `json:"dm_policy"`
	DMAllowFrom *[]string             `json:"dm_allow_from"`
	AllowBots   *string               `json:"allow_bots"`
	// Token: nil = keep current value, "" = remove, non-empty = set.
	BotToken *string `json:"bot_token"`
}

// PUT /api/v1/instances/{id}/discord
//
// Persists the structured Discord config and the token env var, then
// propagates: a token change restarts the container (env vars are injected at
// create time; the boot script re-applies channels.discord from
// OPENCLAW_INITIAL_DISCORD), while a config-only change is pushed live over
// SSH with just a gateway restart.
func UpdateInstanceDiscord(w http.ResponseWriter, r *http.Request) {
	inst, ok := resolveInstanceForChannelSettings(w, r)
	if !ok {
		return
	}

	var body instanceDiscordUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	cfg, _ := parseDiscordConfig(inst.DiscordConfig)
	if body.Enabled != nil {
		cfg.Enabled = *body.Enabled
	}
	if body.Channels != nil {
		cfg.Channels = *body.Channels
	}
	if body.DMPolicy != nil {
		cfg.DMPolicy = *body.DMPolicy
	}
	if body.DMAllowFrom != nil {
		cfg.DMAllowFrom = *body.DMAllowFrom
	}
	if body.AllowBots != nil {
		cfg.AllowBots = *body.AllowBots
	}
	if cfg.Channels == nil {
		cfg.Channels = []discordChannelRule{}
	}
	if err := validateDiscordConfig(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to serialize Discord config")
		return
	}
	configChanged := string(cfgJSON) != inst.DiscordConfig
	if configChanged {
		if err := database.DB.Model(inst).Update("discord_config", string(cfgJSON)).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to save Discord config")
			return
		}
		inst.DiscordConfig = string(cfgJSON)
	}

	// Apply the token change through the encrypted env-vars store.
	envVarsChanged := false
	if body.BotToken != nil {
		set := map[string]string{}
		var unset []string
		if *body.BotToken == "" {
			unset = append(unset, discordBotTokenEnvVar)
		} else {
			set[discordBotTokenEnvVar] = *body.BotToken
		}
		updated, changed, err := ApplyEnvVarsDelta(inst.EnvVars, set, unset)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if changed {
			if err := database.DB.Model(inst).Update("env_vars", updated).Error; err != nil {
				writeError(w, http.StatusInternalServerError, "Failed to save Discord token")
				return
			}
			inst.EnvVars = updated
			envVarsChanged = true
		}
	}

	resp := discordResponseFor(*inst)

	// Propagate to the running container. The bot token is an env var, and
	// env vars only enter a container when its spec is built, so a token
	// change needs a restart; the boot script then re-applies
	// channels.discord from OPENCLAW_INITIAL_DISCORD, which makes the SSH
	// push redundant in that case. A config-only edit is pushed live instead.
	//
	// The restart decision is EnsureEnvPropagated's, not envVarsChanged's: it
	// diffs the live container env, so a token that was saved earlier but
	// never reached the container (agent still provisioning, status column
	// stale) heals here instead of being stuck behind a changed=false no-op
	// forever. envVarsChanged only tells us whether to bother looking.
	// Discord's plugin is not part of OpenClaw any more, so enabling the
	// channel has to put it on the agent first -- auto-enable cannot enable
	// something that was never discovered. Async and best-effort: it is an npm
	// install inside the pod, and the readback on the next card load reports
	// how it went.
	if cfg.Enabled {
		EnsureChannelPluginInstalled(inst.ID, inst.Name, "discord")
	}

	if envVarsChanged || configChanged {
		if EnsureEnvPropagated(r.Context(), *inst, callerID(r), discordBotTokenEnvVar) {
			resp.Restarting = true
		} else if configChanged {
			if rendered := renderInitialDiscordEnv(*inst); rendered != "" {
				pushDiscordConfig(inst.ID, inst.Name, rendered)
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
