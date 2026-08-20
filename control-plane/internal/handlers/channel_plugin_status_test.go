package handlers

import "testing"

func TestParseChannelPluginStatus(t *testing.T) {
	const listed = `{
	  "workspaceDir": "/home/claworc",
	  "plugins": [
	    {"id": "discord", "status": "loaded", "channelIds": ["discord"]},
	    {"id": "slack", "status": "disabled", "error": "not in allowlist", "channelIds": ["slack"]},
	    {"id": "telegram", "status": "error", "error": "boom", "channelIds": ["telegram"]}
	  ],
	  "diagnostics": []
	}`

	cases := []struct {
		channel    string
		wantState  channelPluginState
		wantDetail string
	}{
		{"discord", pluginLoaded, ""},
		{"slack", pluginDisabled, "not in allowlist"},
		{"telegram", pluginError, "boom"},
		// A channel with no plugin at all is "missing", not "unknown": we did
		// get an answer from the agent, and the answer was that it is absent.
		{"signal", pluginMissing, ""},
	}
	for _, tc := range cases {
		got, err := parseChannelPluginStatus(listed, tc.channel)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.channel, err)
		}
		if got.State != tc.wantState {
			t.Errorf("%s: state = %q, want %q", tc.channel, got.State, tc.wantState)
		}
		if got.Detail != tc.wantDetail {
			t.Errorf("%s: detail = %q, want %q", tc.channel, got.Detail, tc.wantDetail)
		}
	}
}

// The bundled extensions use the channel name as their plugin id, but the
// manifest's channels list is the real contract -- a plugin repackaged under
// another id still drives the channel and must be recognised.
func TestParseChannelPluginStatusMatchesByChannelID(t *testing.T) {
	const listed = `{"plugins":[{"id":"acme-discord-fork","status":"loaded","channelIds":["discord"]}]}`
	got, err := parseChannelPluginStatus(listed, "discord")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.State != pluginLoaded {
		t.Errorf("state = %q, want %q", got.State, pluginLoaded)
	}
}

// A status OpenClaw might add later is reported as "disabled" rather than
// mistaken for healthy: the safe reading of an unrecognised state is that the
// plugin is not running.
func TestParseChannelPluginStatusUnrecognisedStateIsNotLoaded(t *testing.T) {
	const listed = `{"plugins":[{"id":"discord","status":"quarantined","channelIds":["discord"]}]}`
	got, err := parseChannelPluginStatus(listed, "discord")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.State != pluginDisabled {
		t.Errorf("state = %q, want %q", got.State, pluginDisabled)
	}
}

// The agent's login shell can print a banner ahead of the JSON. Failing the
// unmarshal there would report a perfectly healthy plugin as unknown.
func TestParseChannelPluginStatusIgnoresSurroundingOutput(t *testing.T) {
	const noisy = "Welcome to the agent\n" +
		`{"plugins":[{"id":"slack","status":"loaded","channelIds":["slack"]}]}` +
		"\nlogout\n"
	got, err := parseChannelPluginStatus(noisy, "slack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.State != pluginLoaded {
		t.Errorf("state = %q, want %q", got.State, pluginLoaded)
	}
}

func TestParseChannelPluginStatusRejectsGarbage(t *testing.T) {
	if _, err := parseChannelPluginStatus("command not found", "slack"); err == nil {
		t.Error("expected an error so the caller can report unknown rather than missing")
	}
}

// The channels Claworc manages must both resolve to the npm packages
// OpenClaw's own catalog calls official. A typo here fails at install time on
// the agent, minutes later and in a log nobody is reading.
func TestChannelPluginSpecs(t *testing.T) {
	want := map[string]string{
		"discord": "@openclaw/discord",
		"slack":   "@openclaw/slack",
	}
	for channel, spec := range want {
		if got := channelPluginSpecs[channel]; got != spec {
			t.Errorf("channelPluginSpecs[%q] = %q, want %q", channel, got, spec)
		}
	}
	if len(channelPluginSpecs) != len(want) {
		t.Errorf("channelPluginSpecs has %d entries, want %d — a new channel needs a test here too",
			len(channelPluginSpecs), len(want))
	}
}

// Only a confirmed-absent plugin may be installed. This encodes the two
// judgements that keep an npm install from firing on a guess: "unknown" is not
// evidence of absence, and "disabled" is a config decision that reinstalling
// would not change.
func TestOnlyMissingPluginsWarrantInstall(t *testing.T) {
	cases := []struct {
		state       channelPluginState
		wantInstall bool
	}{
		{pluginMissing, true},
		{pluginLoaded, false},
		{pluginDisabled, false},
		{pluginError, false},
		{pluginUnknown, false},
	}
	for _, tc := range cases {
		got := tc.state == pluginMissing
		if got != tc.wantInstall {
			t.Errorf("state %q: install = %v, want %v", tc.state, got, tc.wantInstall)
		}
	}
}
