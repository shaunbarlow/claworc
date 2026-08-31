package handlers

import (
	"context"
	"testing"

	"github.com/gluk-w/claworc/control-plane/internal/database"
)

// A per-instance selection overrides the global default in both directions,
// and an unset instance falls back to "legacy" — never a bare "" that would
// reach the agent's config verbatim.
func TestEffectiveContextEnginePrecedence(t *testing.T) {
	setupHandlersTestDB(t)

	if got := effectiveContextEngine(&database.Instance{}); got != "legacy" {
		t.Errorf("no override, no default = %q, want \"legacy\"", got)
	}

	if err := database.SetSetting("default_context_engine", "lossless-claw"); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if got := effectiveContextEngine(&database.Instance{}); got != "lossless-claw" {
		t.Errorf("inherited engine = %q, want \"lossless-claw\"", got)
	}
	if got := effectiveContextEngine(&database.Instance{ContextEngine: "legacy"}); got != "legacy" {
		t.Errorf("explicit override = %q, want \"legacy\"", got)
	}

	// An unknown global value must not be handed to the agent.
	if err := database.SetSetting("default_context_engine", "bogus"); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if got := effectiveContextEngine(&database.Instance{}); got != "legacy" {
		t.Errorf("engine for an invalid global default = %q, want \"legacy\"", got)
	}
}

// parseLosslessClawSettings must reject values outside the ranges OpenClaw's
// own schema enforces (contextThreshold 0-1, sweepMaxDepth >= -1, etc.) so
// operators get a save-time error instead of the agent silently rejecting
// the write later.
func TestParseLosslessClawSettingsValidation(t *testing.T) {
	setupHandlersTestDB(t)

	cases := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{"empty ok", `{}`, false},
		{"valid threshold", `{"context_threshold":0.8}`, false},
		{"threshold too high", `{"context_threshold":1.5}`, true},
		{"threshold negative", `{"context_threshold":-0.1}`, true},
		{"valid sweep depth unlimited", `{"sweep_max_depth":-1}`, false},
		{"sweep depth too low", `{"sweep_max_depth":-2}`, true},
		{"fresh tail zero", `{"fresh_tail_count":0}`, true},
		{"leaf chunk zero", `{"leaf_chunk_tokens":0}`, true},
		{"valid fallback mode", `{"host_fallback_mode":"capture-only"}`, false},
		{"invalid fallback mode", `{"host_fallback_mode":"bogus"}`, true},
		{"fallback provider missing model", `{"fallback_providers":[{"provider":"anthropic"}]}`, true},
		{"valid fallback provider", `{"fallback_providers":[{"provider":"anthropic","model":"claude"}]}`, false},
		{"advanced not object", `{"advanced":[1,2]}`, true},
		{"advanced object ok", `{"advanced":{"maxSweepIterations":3}}`, false},
		{"unknown field rejected", `{"bogus_field":true}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLosslessClawSettings([]byte(tc.json))
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %s, got nil", tc.json)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for %s: %v", tc.json, err)
			}
		})
	}
}

// mergeLosslessClawSettings: set fields win over the global default; unset
// fields inherit — same contract as mergeMemorySettings.
func TestMergeLosslessClawSettings(t *testing.T) {
	threshold := 0.9
	globalThreshold := 0.7
	fresh := 40

	global := LosslessClawSettings{ContextThreshold: &globalThreshold, HostFallbackMode: "error"}
	override := LosslessClawSettings{ContextThreshold: &threshold, FreshTailCount: &fresh}

	merged := mergeLosslessClawSettings(global, override)
	if merged.ContextThreshold == nil || *merged.ContextThreshold != threshold {
		t.Errorf("ContextThreshold = %v, want override value %v", merged.ContextThreshold, threshold)
	}
	if merged.FreshTailCount == nil || *merged.FreshTailCount != fresh {
		t.Errorf("FreshTailCount = %v, want %v", merged.FreshTailCount, fresh)
	}
	if merged.HostFallbackMode != "error" {
		t.Errorf("HostFallbackMode = %q, want inherited \"error\"", merged.HostFallbackMode)
	}
}

// buildLosslessClawConfig renders curated fields using OpenClaw's actual key
// names (confirmed live via `openclaw plugins inspect lossless-claw --json`),
// and the Advanced overlay is deep-merged last without clobbering curated
// keys it doesn't mention.
func TestBuildLosslessClawConfig(t *testing.T) {
	threshold := 0.75
	fresh := 30
	s := LosslessClawSettings{
		ContextThreshold: &threshold,
		FreshTailCount:   &fresh,
		HostFallbackMode: "capture-only",
		Advanced:         []byte(`{"maxSweepIterations":5}`),
	}
	cfg := buildLosslessClawConfig(s)
	if cfg["contextThreshold"] != threshold {
		t.Errorf("contextThreshold = %v, want %v", cfg["contextThreshold"], threshold)
	}
	if cfg["freshTailCount"] != fresh {
		t.Errorf("freshTailCount = %v, want %v", cfg["freshTailCount"], fresh)
	}
	if cfg["hostFallbackMode"] != "capture-only" {
		t.Errorf("hostFallbackMode = %v, want capture-only", cfg["hostFallbackMode"])
	}
	// json.Unmarshal into interface{} decodes numbers as float64, not int.
	if cfg["maxSweepIterations"] != float64(5) {
		t.Errorf("advanced overlay maxSweepIterations = %v, want 5", cfg["maxSweepIterations"])
	}
}

// summaryModel/summaryProvider require the companion llm.allowModelOverride
// trust grant or OpenClaw silently ignores the override — this is the one
// place lossless-claw's config surface needs a second subtree kept in sync.
func TestBuildContextEngineLLMPolicy(t *testing.T) {
	if _, ok := buildContextEngineLLMPolicy(LosslessClawSettings{}); ok {
		t.Error("no summary model/provider set, but a policy was still built")
	}

	s := LosslessClawSettings{SummaryProvider: "anthropic", SummaryModel: "claude-haiku"}
	policy, ok := buildContextEngineLLMPolicy(s)
	if !ok {
		t.Fatal("expected a policy to be built")
	}
	if policy["allowModelOverride"] != true {
		t.Errorf("allowModelOverride = %v, want true", policy["allowModelOverride"])
	}
	models, ok := policy["allowedModels"].([]string)
	if !ok || len(models) != 1 || models[0] != "anthropic/claude-haiku" {
		t.Errorf("allowedModels = %v, want [\"anthropic/claude-haiku\"]", policy["allowedModels"])
	}
}

// applyContextEngineConfig must never restart the gateway for an
// already-installed plugin: every lossless-claw config path reports
// reloadKind "hot" (confirmed live via `gateway config.schema.lookup`).
func TestApplyContextEngineConfigHotReloadsWhenAlreadyInstalled(t *testing.T) {
	setupHandlersTestDB(t)
	inst := &database.Instance{Name: "bot-x", ContextEngine: "lossless-claw"}

	agent := &mockInstance{results: []callResult{
		{stdout: `{"plugins":[{"id":"lossless-claw"}]}`},
	}}
	applyContextEngineConfig(context.Background(), agent, "bot-x", inst)

	if _, ok := findCall(agent.calls, "gateway", "stop"); ok {
		t.Errorf("gateway was restarted for an already-installed plugin; calls: %v", agent.calls)
	}
	if _, ok := findCall(agent.calls, "config", "set", "plugins.slots.contextEngine"); !ok {
		t.Errorf("plugins.slots.contextEngine was never set; calls: %v", agent.calls)
	}
	if argv, ok := findCall(agent.calls, "config", "set", "plugins.entries.lossless-claw.enabled"); !ok {
		t.Errorf("lossless-claw plugin was never enabled; calls: %v", agent.calls)
	} else if argv[3] != "true" {
		t.Errorf("enabled value = %q, want \"true\"", argv[3])
	}
}

// A fresh install does force a restart, since the agent process needs to
// discover the newly installed plugin.
func TestApplyContextEngineConfigInstallsAndRestartsWhenMissing(t *testing.T) {
	setupHandlersTestDB(t)
	inst := &database.Instance{Name: "bot-x", ContextEngine: "lossless-claw"}

	agent := &mockInstance{results: []callResult{
		{stdout: `{"plugins":[]}`}, // plugins list: not installed
		{},                         // plugins install
	}}
	applyContextEngineConfig(context.Background(), agent, "bot-x", inst)

	if _, ok := findCall(agent.calls, "plugins", "install", "@martian-engineering/lossless-claw"); !ok {
		t.Errorf("plugin was never installed; calls: %v", agent.calls)
	}
	if _, ok := findCall(agent.calls, "gateway", "stop"); !ok {
		t.Errorf("gateway was not restarted after a fresh install; calls: %v", agent.calls)
	}
}

// The plugin's own key names use the "provider/model" convention: the
// slot value must be the plain string "lossless-claw", not JSON, mirroring
// tools.web.search.provider's --json footgun in search.go.
func TestApplyContextEngineConfigSlotIsPlainString(t *testing.T) {
	setupHandlersTestDB(t)
	inst := &database.Instance{Name: "bot-x", ContextEngine: "lossless-claw"}

	agent := &mockInstance{results: []callResult{
		{stdout: `{"plugins":[{"id":"lossless-claw"}]}`},
	}}
	applyContextEngineConfig(context.Background(), agent, "bot-x", inst)

	argv, ok := findCall(agent.calls, "config", "set", "plugins.slots.contextEngine")
	if !ok {
		t.Fatalf("slot was never set; calls: %v", agent.calls)
	}
	if hasArg(argv, "--json") {
		t.Errorf("slot set passes --json with a bare string value: %v", argv)
	}
	if argv[3] != "lossless-claw" {
		t.Errorf("slot value = %q, want \"lossless-claw\"", argv[3])
	}
}

// Switching back to legacy has to remove what Claworc pinned — same
// teardown contract as applySearchConfig's auto-switch-back path.
func TestApplyContextEngineConfigTeardownClearsOwnedPaths(t *testing.T) {
	setupHandlersTestDB(t)
	inst := &database.Instance{Name: "bot-x", ContextEngine: "legacy"}

	agent := &mockInstance{}
	applyContextEngineConfig(context.Background(), agent, "bot-x", inst)

	for _, path := range []string{
		"plugins.slots.contextEngine",
		"plugins.entries.lossless-claw.enabled",
		"plugins.entries.lossless-claw.llm",
	} {
		argv, ok := findCall(agent.calls, "config", "unset", path)
		if !ok {
			t.Errorf("%s was never unset; calls: %v", path, agent.calls)
			continue
		}
		if hasArg(argv, "--json") {
			t.Errorf("unset %s passes --json, which OpenClaw refuses: %v", path, argv)
		}
	}

	// Nothing may be *written* on the teardown path.
	if argv, ok := findCall(agent.calls, "config", "set"); ok {
		t.Errorf("teardown wrote config: %v", argv)
	}
}

// One failing unset must not abandon the rest of the teardown.
func TestApplyContextEngineConfigTeardownContinuesAfterFailure(t *testing.T) {
	setupHandlersTestDB(t)
	inst := &database.Instance{Name: "bot-x", ContextEngine: ""}

	agent := &mockInstance{results: []callResult{
		{stderr: "Config write rejected (size-drop:0.42)", code: 1},
	}}
	applyContextEngineConfig(context.Background(), agent, "bot-x", inst)

	if _, ok := findCall(agent.calls, "config", "unset", "plugins.entries.lossless-claw.enabled"); !ok {
		t.Errorf("a failed unset aborted the rest of the teardown; calls: %v", agent.calls)
	}
}
