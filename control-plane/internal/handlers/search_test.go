package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

// findCall returns the first recorded ExecOpenclaw argv whose leading elements
// match want, plus whether one was found. It matches on a prefix so callers can
// assert on `config set <path>` without pinning the value or trailing flags.
func findCall(calls [][]string, want ...string) ([]string, bool) {
	for _, got := range calls {
		if len(got) < len(want) {
			continue
		}
		match := true
		for i := range want {
			if got[i] != want[i] {
				match = false
				break
			}
		}
		if match {
			return got, true
		}
	}
	return nil, false
}

func hasArg(argv []string, arg string) bool {
	for _, a := range argv {
		if a == arg {
			return true
		}
	}
	return false
}

// seedBraveInstance returns an instance pinned to Brave, with the global key
// set so effectiveBraveAPIKeyPlain resolves.
func seedBraveInstance(t *testing.T) *database.Instance {
	t.Helper()
	enc, err := utils.Encrypt("test-brave-key")
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	if err := database.SetSetting("brave_api_key", enc); err != nil {
		t.Fatalf("set brave_api_key: %v", err)
	}
	return &database.Instance{Name: "bot-x", SearchProvider: "brave"}
}

// buildSearchConfig must never emit an apiKey SecretRef. An unresolved
// SecretRef is startup-fatal for the whole gateway
// ("[WEB_SEARCH_KEY_UNRESOLVED_NO_FALLBACK] ... SecretRef is unresolved"), and
// the config outlives the container env it would point at, so a ref written
// before BRAVE_API_KEY reaches the container bricks the agent. The Brave
// plugin reads the env var itself, so the ref buys nothing.
func TestBuildSearchConfigNeverEmitsSecretRef(t *testing.T) {
	setupHandlersTestDB(t)
	inst := seedBraveInstance(t)

	// Sanity: the key really is resolvable, so this is not a vacuous pass.
	if got := effectiveBraveAPIKeyPlain(inst); got != "test-brave-key" {
		t.Fatalf("effectiveBraveAPIKeyPlain = %q, want the seeded key", got)
	}

	provider, cfg, ok := buildSearchConfig(inst)
	if !ok || provider != "brave" {
		t.Fatalf("buildSearchConfig = (%q, ok=%v), want (brave, true)", provider, ok)
	}
	if _, present := cfg["apiKey"]; present {
		t.Errorf("webSearch config carries an apiKey (%v); the key must only ever reach the agent as the BRAVE_API_KEY env var", cfg["apiKey"])
	}
	if cfg["mode"] != "web" {
		t.Errorf("webSearch mode = %v, want \"web\"", cfg["mode"])
	}
}

// The key reaches the agent through the container env and nowhere else.
func TestBuildSearchEnvVars(t *testing.T) {
	setupHandlersTestDB(t)
	inst := seedBraveInstance(t)

	if got := buildSearchEnvVars(inst)["BRAVE_API_KEY"]; got != "test-brave-key" {
		t.Errorf("BRAVE_API_KEY = %q, want the resolved key", got)
	}

	// No managed provider selected: leave any user-set BRAVE_API_KEY alone.
	inst.SearchProvider = ""
	if env := buildSearchEnvVars(inst); len(env) != 0 {
		t.Errorf("env vars for an unmanaged instance = %v, want empty", env)
	}
}

// Guards the bug that made the feature look dead: `config set` with --json is
// --strict-json, so a bare string value is rejected outright
// ("Could not parse \"brave\" as JSON") and tools.web.search.provider was
// never written. JSON-valued paths must still pass --json.
func TestApplySearchConfigFlagsMatchValueTypes(t *testing.T) {
	setupHandlersTestDB(t)
	inst := seedBraveInstance(t)

	// Report the brave plugin as already installed so no install/restart runs.
	agent := &mockInstance{results: []callResult{
		{stdout: `{"plugins":[{"id":"brave"}]}`},
	}}
	applySearchConfig(context.Background(), agent, "bot-x", inst)

	argv, ok := findCall(agent.calls, "config", "set", "tools.web.search.provider")
	if !ok {
		t.Fatalf("tools.web.search.provider was never set; calls: %v", agent.calls)
	}
	if hasArg(argv, "--json") {
		t.Errorf("provider set passes --json with the bare string value %q; OpenClaw rejects that write: %v", "brave", argv)
	}
	if argv[3] != "brave" {
		t.Errorf("provider value = %q, want \"brave\"", argv[3])
	}

	// The webSearch subtree is genuine JSON and must keep --json (and
	// --replace, so paths Claworc drops disappear).
	argv, ok = findCall(agent.calls, "config", "set", "plugins.entries.brave.config.webSearch")
	if !ok {
		t.Fatalf("webSearch config was never set; calls: %v", agent.calls)
	}
	if !hasArg(argv, "--json") || !hasArg(argv, "--replace") {
		t.Errorf("webSearch set = %v, want --json and --replace", argv)
	}
	if strings.Contains(argv[3], "apiKey") {
		t.Errorf("webSearch payload leaks a key reference: %s", argv[3])
	}

	// The plugin must be switched on, or the provider is unavailable.
	if argv, ok := findCall(agent.calls, "config", "set", "plugins.entries.brave.enabled"); !ok {
		t.Errorf("brave plugin was never enabled; calls: %v", agent.calls)
	} else if argv[3] != "true" {
		t.Errorf("enabled value = %q, want \"true\"", argv[3])
	}
}

// Switching back to auto has to remove what Claworc pinned. The teardown used
// to pass --json to `config unset`, which OpenClaw refuses outright
// ("--json can only be used with --dry-run"), so nothing was ever cleared.
func TestApplySearchConfigTeardownClearsOwnedPaths(t *testing.T) {
	setupHandlersTestDB(t)
	inst := &database.Instance{Name: "bot-x", SearchProvider: ""}

	agent := &mockInstance{}
	applySearchConfig(context.Background(), agent, "bot-x", inst)

	for _, path := range []string{
		"tools.web.search.provider",
		"plugins.entries.brave.enabled",
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

// `config unset` exits non-zero with "Config path not found" for a path that
// was never set — the normal case for an agent that never had a managed
// provider. That is success (the path is gone, which is all the caller wants),
// not a failure, and it must not stop the remaining unsets. A genuine failure
// must still be reported.
func TestSearchConfigUnsetTreatsAbsentPathAsSuccess(t *testing.T) {
	notFound := &mockInstance{results: []callResult{
		{stderr: "Config path not found: tools.web.search.provider. Nothing was changed.", code: 1},
	}}
	if !searchConfigUnset(context.Background(), notFound, "bot-x", "tools.web.search.provider") {
		t.Error("an already-absent path reported failure; the path is gone, which is the goal")
	}

	realFailure := &mockInstance{results: []callResult{
		{stderr: "Config write rejected (size-drop:0.42)", code: 1},
	}}
	if searchConfigUnset(context.Background(), realFailure, "bot-x", "tools.web.search.provider") {
		t.Error("a rejected write reported success")
	}
}

// One failing unset must not abandon the rest of the teardown.
func TestApplySearchConfigTeardownContinuesAfterFailure(t *testing.T) {
	setupHandlersTestDB(t)
	inst := &database.Instance{Name: "bot-x", SearchProvider: ""}

	agent := &mockInstance{results: []callResult{
		{stderr: "Config write rejected (size-drop:0.42)", code: 1},
	}}
	applySearchConfig(context.Background(), agent, "bot-x", inst)

	if _, ok := findCall(agent.calls, "config", "unset", "plugins.entries.brave.enabled"); !ok {
		t.Errorf("a failed unset aborted the rest of the teardown; calls: %v", agent.calls)
	}
}

// A per-instance selection overrides the global default in both directions.
func TestEffectiveSearchProviderPrecedence(t *testing.T) {
	setupHandlersTestDB(t)
	if err := database.SetSetting("default_search_provider", "brave"); err != nil {
		t.Fatalf("set default: %v", err)
	}

	if got := effectiveSearchProvider(&database.Instance{}); got != "brave" {
		t.Errorf("inherited provider = %q, want \"brave\"", got)
	}
	if got := effectiveSearchProvider(&database.Instance{SearchProvider: "brave"}); got != "brave" {
		t.Errorf("explicit provider = %q, want \"brave\"", got)
	}

	// An unknown global value must not be handed to the agent.
	if err := database.SetSetting("default_search_provider", "bogus"); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if got := effectiveSearchProvider(&database.Instance{}); got != "" {
		t.Errorf("provider for an invalid global default = %q, want \"\"", got)
	}
}
