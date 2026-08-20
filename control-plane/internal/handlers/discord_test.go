package handlers

import (
	"encoding/json"
	"testing"
)

func TestValidateDiscordConfigNormalizes(t *testing.T) {
	f := false
	cfg := instanceDiscordConfig{
		Enabled: true,
		Channels: []discordChannelRule{
			{GuildID: " 123456789012345678 ", ChannelID: "#234567890123456789"},
			{GuildID: "123456789012345678", ChannelID: "234567890123456789", RequireMention: &f}, // dup after normalization
			{GuildID: "999999999999999999"}, // whole guild
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
		{Enabled: true, DMPolicy: "allowlist"},
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
	if c1["allow"] != true || c1["requireMention"] != true {
		t.Errorf("default channel entry should be allow+requireMention true, got %v", c1)
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
