package handlers

import (
	"encoding/json"
	"testing"
)

func TestValidateSlackConfigNormalizesChannelIDs(t *testing.T) {
	f := false
	cfg := instanceSlackConfig{
		Enabled: true,
		Channels: []slackChannelEntry{
			{ID: " #c0123456789 "},
			{ID: "C0123456789", RequireMention: &f}, // duplicate after normalization
			{ID: "G9876543210"},
		},
	}
	if err := validateSlackConfig(&cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Channels) != 2 {
		t.Fatalf("expected duplicate channel to be dropped, got %d entries", len(cfg.Channels))
	}
	if cfg.Channels[0].ID != "C0123456789" || cfg.Channels[1].ID != "G9876543210" {
		t.Fatalf("unexpected normalized IDs: %+v", cfg.Channels)
	}
}

func TestValidateSlackConfigRejectsChannelNames(t *testing.T) {
	for _, bad := range []string{"general", "#general", "my-channel", "D0123456789", ""} {
		cfg := instanceSlackConfig{Enabled: true, Channels: []slackChannelEntry{{ID: bad}}}
		if err := validateSlackConfig(&cfg); err == nil {
			t.Errorf("expected error for channel ID %q", bad)
		}
	}
}

func TestValidateSlackConfigRejectsBadDMPolicy(t *testing.T) {
	// "allowlist" used to be the example of an unsupported policy here. It is
	// supported now, so this needs a value that is genuinely not a policy --
	// otherwise the test passes for the wrong reason (an allowlist policy with
	// no users is rejected, but on a different rule).
	cfg := instanceSlackConfig{Enabled: true, DMPolicy: "everyone"}
	if err := validateSlackConfig(&cfg); err == nil {
		t.Error("expected error for unsupported dm_policy")
	}
}

func TestRenderSlackChannelsJSON(t *testing.T) {
	f := false
	cfg := instanceSlackConfig{
		Enabled: true,
		Channels: []slackChannelEntry{
			{ID: "C0123456789"},
			{ID: "G9876543210", RequireMention: &f},
		},
		DMPolicy: "open",
	}
	rendered, err := renderSlackChannelsJSON(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var block map[string]interface{}
	if err := json.Unmarshal([]byte(rendered), &block); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v", err)
	}
	if block["enabled"] != true || block["groupPolicy"] != "allowlist" {
		t.Fatalf("unexpected top-level block: %v", block)
	}
	channels, ok := block["channels"].(map[string]interface{})
	if !ok || len(channels) != 2 {
		t.Fatalf("expected 2 channels, got %v", block["channels"])
	}
	c1 := channels["C0123456789"].(map[string]interface{})
	if c1["requireMention"] != true {
		t.Errorf("nil RequireMention should render as true (OpenClaw default), got %v", c1)
	}
	c2 := channels["G9876543210"].(map[string]interface{})
	if c2["requireMention"] != false {
		t.Errorf("explicit false RequireMention should render as false, got %v", c2)
	}
	if block["dmPolicy"] != "open" {
		t.Errorf("expected dmPolicy open, got %v", block["dmPolicy"])
	}
	allowFrom, ok := block["allowFrom"].([]interface{})
	if !ok || len(allowFrom) != 1 || allowFrom[0] != "*" {
		t.Errorf("open dmPolicy must render allowFrom [\"*\"], got %v", block["allowFrom"])
	}
}

func TestRenderSlackChannelsJSONDisabled(t *testing.T) {
	rendered, err := renderSlackChannelsJSON(instanceSlackConfig{
		Enabled:  false,
		Channels: []slackChannelEntry{{ID: "C0123456789"}},
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
// the pairing handshake, so the member IDs have to reach OpenClaw as
// allowFrom -- the same field "open" fills with "*".
func TestRenderSlackChannelsJSONDMAllowlist(t *testing.T) {
	rendered, err := renderSlackChannelsJSON(instanceSlackConfig{
		Enabled:     true,
		Channels:    []slackChannelEntry{},
		DMPolicy:    "allowlist",
		DMAllowFrom: []string{"U0123456789", "W0987654321"},
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
	if allowFrom[0] != "U0123456789" || allowFrom[1] != "W0987654321" {
		t.Errorf("allowFrom should carry the member IDs in order, got %v", allowFrom)
	}
}

func TestValidateSlackConfigDMAllowlist(t *testing.T) {
	// Mentions are what you get from copying a user in Slack, so accept them
	// and normalize down to the bare member ID.
	cfg := instanceSlackConfig{
		DMPolicy:    "allowlist",
		DMAllowFrom: []string{" <@U0123456789> ", "u0987654321", "U0123456789", ""},
	}
	if err := validateSlackConfig(&cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"U0123456789", "U0987654321"}
	if len(cfg.DMAllowFrom) != len(want) {
		t.Fatalf("expected %v after normalization, got %v", want, cfg.DMAllowFrom)
	}
	for i, id := range want {
		if cfg.DMAllowFrom[i] != id {
			t.Errorf("DMAllowFrom[%d] = %q, want %q", i, cfg.DMAllowFrom[i], id)
		}
	}

	// A display name is mutable and ambiguous; insist on the stable ID, same
	// line this package already takes for channel names. The digit rule is
	// what stops a name beginning with U or W from slipping through.
	for _, bad := range []string{"shaun", "ursula", "@shaun"} {
		cfg := instanceSlackConfig{DMPolicy: "allowlist", DMAllowFrom: []string{bad}}
		if err := validateSlackConfig(&cfg); err == nil {
			t.Errorf("expected %q to be rejected in favour of the member ID", bad)
		}
	}

	// An empty allowlist blocks every DM while reading as "some users are
	// allowed" -- that is "disabled" under the wrong name.
	empty := instanceSlackConfig{DMPolicy: "allowlist"}
	if err := validateSlackConfig(&empty); err == nil {
		t.Error("expected an empty allowlist to be rejected")
	}

	// The list survives a switch away from allowlist, so toggling policies in
	// the UI does not throw the user's work away.
	kept := instanceSlackConfig{DMPolicy: "pairing", DMAllowFrom: []string{"U0123456789"}}
	if err := validateSlackConfig(&kept); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kept.DMAllowFrom) != 1 {
		t.Errorf("allowlist should be preserved under other policies, got %v", kept.DMAllowFrom)
	}
}
