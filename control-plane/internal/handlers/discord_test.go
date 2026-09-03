package handlers

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestValidateDiscordConfigNormalizes(t *testing.T) {
	f := false
	cfg := instanceDiscordConfig{
		Enabled: true,
		Channels: []discordChannelRule{
			{GuildID: " 123456789012345678 ", ChannelID: "#234567890123456789"},
			{GuildID: "123456789012345678", ChannelID: "234567890123456789", RequireMention: &f}, // dup after normalization
			{GuildID: "999999999999999999"},                                                      // whole guild
		},
	}
	if err := validateDiscordConfig(&cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Channels) != 2 {
		t.Fatalf("expected duplicate to be dropped, got %d entries", len(cfg.Channels))
	}
	if cfg.Channels[0].GuildID != "123456789012345678" || cfg.Channels[0].ChannelID != "234567890123456789" {
		t.Fatalf("unexpected normalized entry: %+v", cfg.Channels[0])
	}
}

func TestValidateDiscordConfigRejectsBadInput(t *testing.T) {
	cases := []instanceDiscordConfig{
		{Enabled: true, Channels: []discordChannelRule{{GuildID: "my-server"}}},
		{Enabled: true, Channels: []discordChannelRule{{GuildID: ""}}},
		{Enabled: true, Channels: []discordChannelRule{{GuildID: "123456789012345678", ChannelID: "general"}}},
		// whole-guild + specific channel for the same guild
		{Enabled: true, Channels: []discordChannelRule{
			{GuildID: "123456789012345678"},
			{GuildID: "123456789012345678", ChannelID: "234567890123456789"},
		}},
		// "allowlist" used to be the example of an unsupported policy here. It
		// is supported now, so this needs a value that is genuinely not a
		// policy -- an allowlist with no users is still rejected, but by a
		// different rule (see TestValidateDiscordConfigDMAllowlist).
		{Enabled: true, DMPolicy: "everyone"},
		{Enabled: true, AllowBots: "always"},
	}
	for i, cfg := range cases {
		if err := validateDiscordConfig(&cfg); err == nil {
			t.Errorf("case %d: expected error, got none", i)
		}
	}
}

func TestRenderDiscordChannelsJSON(t *testing.T) {
	f := false
	cfg := instanceDiscordConfig{
		Enabled: true,
		Channels: []discordChannelRule{
			{GuildID: "123456789012345678", ChannelID: "234567890123456789"},
			{GuildID: "123456789012345678", ChannelID: "345678901234567890", RequireMention: &f},
			{GuildID: "999999999999999999", RequireMention: &f}, // whole guild, no mention needed
		},
		DMPolicy: "open",
	}
	rendered, err := renderDiscordChannelsJSON(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var block map[string]interface{}
	if err := json.Unmarshal([]byte(rendered), &block); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v", err)
	}
	if block["enabled"] != true || block["groupPolicy"] != "allowlist" {
		t.Fatalf("groupPolicy must always be explicit allowlist (Discord defaults to open), got %v", block)
	}
	guilds := block["guilds"].(map[string]interface{})
	g1 := guilds["123456789012345678"].(map[string]interface{})
	channels := g1["channels"].(map[string]interface{})
	if len(channels) != 2 {
		t.Fatalf("expected 2 channels in guild, got %v", channels)
	}
	c1 := channels["234567890123456789"].(map[string]interface{})
	if c1["enabled"] != true || c1["requireMention"] != true {
		t.Errorf("default channel entry should be enabled+requireMention true, got %v", c1)
	}
	c2 := channels["345678901234567890"].(map[string]interface{})
	if c2["requireMention"] != false {
		t.Errorf("explicit false RequireMention should render false, got %v", c2)
	}
	g2 := guilds["999999999999999999"].(map[string]interface{})
	if g2["requireMention"] != false {
		t.Errorf("whole-guild entry should carry guild-level requireMention, got %v", g2)
	}
	if _, hasChannels := g2["channels"]; hasChannels {
		t.Errorf("whole-guild entry must not render a channels map, got %v", g2)
	}
	if block["dmPolicy"] != "open" {
		t.Errorf("expected dmPolicy open, got %v", block["dmPolicy"])
	}
	allowFrom, ok := block["allowFrom"].([]interface{})
	if !ok || len(allowFrom) != 1 || allowFrom[0] != "*" {
		t.Errorf("open dmPolicy must render allowFrom [\"*\"], got %v", block["allowFrom"])
	}
}

func TestRenderDiscordChannelsJSONDisabled(t *testing.T) {
	rendered, err := renderDiscordChannelsJSON(instanceDiscordConfig{
		Enabled:  false,
		Channels: []discordChannelRule{{GuildID: "123456789012345678"}},
		DMPolicy: "open",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rendered != `{"enabled":false}` {
		t.Fatalf("disabled config must render only the enabled flag, got %s", rendered)
	}
}

// The "allowlist" DM policy is the one that lets named users through without
// the pairing handshake, so the user IDs have to reach OpenClaw as allowFrom --
// the same field "open" fills with "*".
func TestRenderDiscordChannelsJSONDMAllowlist(t *testing.T) {
	rendered, err := renderDiscordChannelsJSON(instanceDiscordConfig{
		Enabled:     true,
		Channels:    []discordChannelRule{},
		DMPolicy:    "allowlist",
		DMAllowFrom: []string{"111111111111111111", "222222222222222222"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var block map[string]interface{}
	if err := json.Unmarshal([]byte(rendered), &block); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v", err)
	}
	if block["dmPolicy"] != "allowlist" {
		t.Errorf("expected dmPolicy allowlist, got %v", block["dmPolicy"])
	}
	allowFrom, ok := block["allowFrom"].([]interface{})
	if !ok || len(allowFrom) != 2 {
		t.Fatalf("expected 2 allowed users, got %v", block["allowFrom"])
	}
	if allowFrom[0] != "111111111111111111" || allowFrom[1] != "222222222222222222" {
		t.Errorf("allowFrom should carry the user IDs in order, got %v", allowFrom)
	}
}

func TestValidateDiscordConfigAllowBots(t *testing.T) {
	for _, good := range []string{"", "true", "mentions"} {
		cfg := instanceDiscordConfig{AllowBots: good}
		if err := validateDiscordConfig(&cfg); err != nil {
			t.Errorf("allow_bots %q: unexpected error: %v", good, err)
		}
	}
	cfg := instanceDiscordConfig{AllowBots: "always"}
	if err := validateDiscordConfig(&cfg); err == nil {
		t.Error("expected an invalid allow_bots value to be rejected")
	}
}

func TestRenderDiscordChannelsJSONAllowBots(t *testing.T) {
	// Default ("") omits the key entirely -- OpenClaw's own default is false.
	rendered, err := renderDiscordChannelsJSON(instanceDiscordConfig{Enabled: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var block map[string]interface{}
	if err := json.Unmarshal([]byte(rendered), &block); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v", err)
	}
	if _, ok := block["allowBots"]; ok {
		t.Errorf("default allow_bots should omit the key, got %v", block)
	}

	rendered, err = renderDiscordChannelsJSON(instanceDiscordConfig{Enabled: true, AllowBots: "true"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := json.Unmarshal([]byte(rendered), &block); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v", err)
	}
	if block["allowBots"] != true {
		t.Errorf("allow_bots \"true\" should render allowBots: true, got %v", block["allowBots"])
	}

	rendered, err = renderDiscordChannelsJSON(instanceDiscordConfig{Enabled: true, AllowBots: "mentions"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := json.Unmarshal([]byte(rendered), &block); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v", err)
	}
	if block["allowBots"] != "mentions" {
		t.Errorf("allow_bots \"mentions\" should render allowBots: \"mentions\", got %v", block["allowBots"])
	}
}

func TestValidateDiscordConfigDMAllowlist(t *testing.T) {
	// Mentions are what you get from copying a user in Discord, so accept them
	// and normalize down to the bare snowflake.
	cfg := instanceDiscordConfig{
		DMPolicy:    "allowlist",
		DMAllowFrom: []string{" <@111111111111111111> ", "<@!222222222222222222>", "111111111111111111", ""},
	}
	if err := validateDiscordConfig(&cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"111111111111111111", "222222222222222222"}
	if len(cfg.DMAllowFrom) != len(want) {
		t.Fatalf("expected %v after normalization, got %v", want, cfg.DMAllowFrom)
	}
	for i, id := range want {
		if cfg.DMAllowFrom[i] != id {
			t.Errorf("DMAllowFrom[%d] = %q, want %q", i, cfg.DMAllowFrom[i], id)
		}
	}

	// A username is silently unroutable, same as for guilds and channels.
	bad := instanceDiscordConfig{DMPolicy: "allowlist", DMAllowFrom: []string{"someuser"}}
	if err := validateDiscordConfig(&bad); err == nil {
		t.Error("expected a username to be rejected in favour of the numeric ID")
	}

	// An empty allowlist blocks every DM while reading as "some users are
	// allowed" -- that is "disabled" under the wrong name.
	empty := instanceDiscordConfig{DMPolicy: "allowlist"}
	if err := validateDiscordConfig(&empty); err == nil {
		t.Error("expected an empty allowlist to be rejected")
	}

	// The list survives a switch away from allowlist, so toggling policies in
	// the UI does not throw the user's work away.
	kept := instanceDiscordConfig{DMPolicy: "pairing", DMAllowFrom: []string{"111111111111111111"}}
	if err := validateDiscordConfig(&kept); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kept.DMAllowFrom) != 1 {
		t.Errorf("allowlist should be preserved under other policies, got %v", kept.DMAllowFrom)
	}
}

// discordSchemaProperties mirrors the @openclaw/discord plugin's config schema
// (channelConfigs.discord.schema in its openclaw.plugin.json), which is strict
// -- every level sets additionalProperties:false, so one unknown key fails
// validation for the *whole* config and the agent starts with no Discord at
// all.
//
// This caught us once already: the bundled schema took `allow` on a guild
// channel entry, the split-out package takes `enabled`, and we kept sending
// `allow`. Re-derive after a plugin bump with:
//
//	npm view @openclaw/discord dist.tarball    # download, extract
//	jq '.channelConfigs.discord.schema.properties | keys' openclaw.plugin.json
var discordSchemaProperties = map[string]bool{
	"enabled": true, "groupPolicy": true, "guilds": true,
	"dmPolicy": true, "allowFrom": true, "token": true, "accounts": true,
	"allowBots": true,
}

var discordGuildProperties = map[string]bool{
	"slug": true, "requireMention": true, "ignoreOtherMentions": true,
	"tools": true, "toolsBySender": true, "reactionNotifications": true,
	"users": true, "roles": true, "channels": true,
}

var discordGuildChannelProperties = map[string]bool{
	"enabled": true, "requireMention": true, "ignoreOtherMentions": true,
	"tools": true, "toolsBySender": true, "skills": true, "systemPrompt": true,
	"users": true, "roles": true, "includeThreadStarter": true,
	"autoThread": true, "autoThreadName": true, "autoArchiveDuration": true,
}

// TestRenderDiscordChannelsJSONMatchesPluginSchema renders every shape we can
// produce and asserts each key is one the plugin accepts.
func TestRenderDiscordChannelsJSONMatchesPluginSchema(t *testing.T) {
	f := false
	configs := []instanceDiscordConfig{
		{Enabled: true, Channels: []discordChannelRule{{GuildID: "111111111111111111", ChannelID: "222222222222222222"}}},
		{Enabled: true, Channels: []discordChannelRule{{GuildID: "111111111111111111", RequireMention: &f}}},
		{Enabled: true, DMPolicy: "open"},
		{Enabled: true, DMPolicy: "allowlist", DMAllowFrom: []string{"333333333333333333"}},
		{Enabled: true, DMPolicy: "disabled"},
		{Enabled: false},
	}
	for i, cfg := range configs {
		rendered, err := renderDiscordChannelsJSON(cfg)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		var block map[string]interface{}
		if err := json.Unmarshal([]byte(rendered), &block); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		for key := range block {
			if !discordSchemaProperties[key] {
				t.Errorf("case %d: channels.discord.%s is not in the plugin schema", i, key)
			}
		}
		guilds, _ := block["guilds"].(map[string]interface{})
		for gid, raw := range guilds {
			guild, _ := raw.(map[string]interface{})
			for key := range guild {
				if !discordGuildProperties[key] {
					t.Errorf("case %d: guilds.%s.%s is not in the plugin schema", i, gid, key)
				}
			}
			chans, _ := guild["channels"].(map[string]interface{})
			for cid, rawCh := range chans {
				entry, _ := rawCh.(map[string]interface{})
				for key := range entry {
					if !discordGuildChannelProperties[key] {
						t.Errorf("case %d: guilds.%s.channels.%s.%s is not in the plugin schema", i, gid, cid, key)
					}
				}
			}
		}
	}
}

// TestApplyDiscordConfig_Argv pins the write shape for channels.discord: a single atomic
// `config set … --replace`, never `config unset` followed by a set. Unsetting
// is a separate write that OpenClaw's size-drop guard rejects on a realistic
// config, and on a config large enough for it to land, a failing set would
// leave the agent with no Discord config at all.
func TestApplyDiscordConfig_Argv(t *testing.T) {
	mock := &mockInstance{}
	applyDiscordConfig(context.Background(), mock, "bot-x", `{"enabled":true}`)

	if len(mock.calls) != 2 {
		t.Fatalf("calls = %d (%v), want set + gateway stop", len(mock.calls), mock.calls)
	}
	for _, c := range mock.calls {
		if len(c) > 1 && c[1] == "unset" {
			t.Errorf("config unset must not be used; got %v", c)
		}
	}
	want := []string{"config", "set", "channels.discord", `{"enabled":true}`, "--replace", "--json"}
	if !reflect.DeepEqual(mock.calls[0], want) {
		t.Errorf("call[0] = %v, want %v", mock.calls[0], want)
	}
	if !reflect.DeepEqual(mock.calls[1], []string{"gateway", "stop", "--force"}) {
		t.Errorf("call[1] = %v, want gateway restart", mock.calls[1])
	}
}

// TestApplyDiscordConfig_SetFailureSkipsRestart: a rejected write must not be
// followed by a gateway restart, and must leave the previous config in place.
func TestApplyDiscordConfig_SetFailureSkipsRestart(t *testing.T) {
	mock := &mockInstance{results: []callResult{{code: 1, stderr: "invalid value"}}}
	applyDiscordConfig(context.Background(), mock, "bot-x", `{"enabled":true}`)
	if len(mock.calls) != 1 {
		t.Fatalf("calls = %v, want only the failed set", mock.calls)
	}
}
