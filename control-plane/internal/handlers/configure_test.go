package handlers

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/orchestrator"
)

// mockInstance records ExecOpenclaw calls and returns queued results.
type mockInstance struct {
	mu      sync.Mutex
	calls   [][]string
	results []callResult
}

type callResult struct {
	stdout, stderr string
	code           int
	err            error
}

func (m *mockInstance) ExecOpenclaw(_ context.Context, args ...string) (string, string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, args)
	if len(m.results) == 0 {
		return "", "", 0, nil
	}
	r := m.results[0]
	if len(m.results) > 1 {
		m.results = m.results[1:]
	}
	return r.stdout, r.stderr, r.code, r.err
}

// mockOps implements orchestrator.ContainerOrchestrator for tests.
type mockOps struct{}

func (mockOps) Initialize(_ context.Context) error                                  { return nil }
func (mockOps) IsAvailable(_ context.Context) bool                                  { return true }
func (mockOps) BackendName() string                                                 { return "mock" }
func (mockOps) CreateInstance(_ context.Context, _ orchestrator.CreateParams) error { return nil }
func (mockOps) DeleteInstance(_ context.Context, _ string) error                    { return nil }
func (mockOps) StartInstance(_ context.Context, _ string) error                     { return nil }
func (mockOps) StopInstance(_ context.Context, _ string) error                      { return nil }
func (mockOps) RestartInstance(_ context.Context, _ string, _ orchestrator.CreateParams) error {
	return nil
}
func (mockOps) GetInstanceEnv(_ context.Context, _ string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (mockOps) GetInstanceStatus(_ context.Context, _ string) (string, error)    { return "running", nil }
func (mockOps) GetInstanceImageInfo(_ context.Context, _ string) (string, error) { return "", nil }
func (mockOps) UpdateInstanceConfig(_ context.Context, _ string, _ string) error { return nil }
func (mockOps) CloneVolumes(_ context.Context, _, _ string) error                { return nil }
func (mockOps) ConfigureSSHAccess(_ context.Context, _ uint, _ string) error     { return nil }
func (mockOps) GetSSHAddress(_ context.Context, _ uint) (string, int, error)     { return "", 0, nil }
func (mockOps) UpdateResources(_ context.Context, _ string, _ orchestrator.UpdateResourcesParams) error {
	return nil
}
func (mockOps) GetContainerStats(_ context.Context, _ string) (*orchestrator.ContainerStats, error) {
	return nil, nil
}
func (mockOps) UpdateImage(_ context.Context, _ string, _ orchestrator.CreateParams) error {
	return nil
}
func (mockOps) ExecInInstance(_ context.Context, _ string, _ []string) (string, string, int, error) {
	return "", "", 0, nil
}
func (mockOps) StreamExecInInstance(_ context.Context, _ string, _ []string, _ io.Writer) (string, int, error) {
	return "", 0, nil
}
func (mockOps) UpdatePlacementConfig(_ context.Context, _ string, _ orchestrator.UpdatePlacementParams) error {
	return nil
}
func (mockOps) DeleteSharedVolume(_ context.Context, _ uint) error         { return nil }
func (mockOps) CloneVolume(_ context.Context, _, _ string) error           { return nil }
func (mockOps) VolumeNameFor(name, suffix string) string                   { return name + "-" + suffix }
func (mockOps) Apply(_ context.Context, _ orchestrator.WorkloadSpec) error { return nil }
func (mockOps) DeleteWorkload(_ context.Context, _ orchestrator.WorkloadSpec) error {
	return nil
}
func (mockOps) EnsureSSHAccess(_ context.Context, _, _ string) error { return nil }
func (mockOps) WorkloadSSHAddress(_ context.Context, _ string) (string, int, error) {
	return "", 0, nil
}
func (mockOps) WorkloadAddress(_ context.Context, _ string, _ int) (string, int, error) {
	return "", 0, nil
}
func (mockOps) SelfUpdate(_ context.Context, _ string) (bool, error) { return false, nil }

func containsArg(call []string, argument string) bool {
	for _, value := range call {
		if value == argument {
			return true
		}
	}
	return false
}

func configBatchValue(t *testing.T, call []string, path string) (json.RawMessage, bool) {
	t.Helper()
	if len(call) < 5 || call[0] != "config" || call[1] != "set" || call[2] != "--batch-json" {
		return nil, false
	}
	var operations []struct {
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(call[3]), &operations); err != nil {
		t.Fatalf("invalid config batch JSON: %v", err)
	}
	for _, operation := range operations {
		if operation.Path == path {
			return operation.Value, true
		}
	}
	return nil, false
}

func configBatchCall(t *testing.T, calls [][]string) []string {
	t.Helper()
	for _, call := range calls {
		if len(call) >= 4 && call[0] == "config" && call[1] == "set" && call[2] == "--batch-json" {
			return call
		}
	}
	t.Fatalf("atomic config batch call not found: %v", calls)
	return nil
}

func TestConfigureInstance_NoOp(t *testing.T) {
	inst := &mockInstance{}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test", nil, nil, 0)
	if len(inst.calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(inst.calls))
	}
}

func TestConfigureInstance_AppliesModelStateAtomically(t *testing.T) {
	inst := &mockInstance{}
	providers := map[string]GatewayProvider{
		"test-openai": {Key: "vk-test", APIType: "openai-completions", Models: []database.ProviderModel{{ID: "test-model", Name: "Test Model"}}},
	}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test", []string{"test-openai/test-model"}, providers, 40001)
	if len(inst.calls) != 2 {
		t.Fatalf("expected one atomic config write and a gateway restart, got %v", inst.calls)
	}
	batch := configBatchCall(t, inst.calls)
	if !containsArg(batch, "--replace") {
		t.Errorf("batch must replace protected paths: %v", batch)
	}
	for _, path := range []string{"models.providers", "agents.defaults.model", "agents.defaults.models", "agents.defaults.modelPolicy.allow"} {
		if _, ok := configBatchValue(t, batch, path); !ok {
			t.Errorf("batch is missing %s: %v", path, batch)
		}
	}
	model, _ := configBatchValue(t, batch, "agents.defaults.model")
	if !strings.Contains(string(model), "test-openai/test-model") {
		t.Errorf("primary model missing from batch: %s", model)
	}
	last := inst.calls[len(inst.calls)-1]
	if last[0] != "gateway" || last[1] != "stop" || !containsArg(last, "--force") {
		t.Errorf("gateway restart must follow a successful batch, got %v", last)
	}
}

func TestConfigureInstance_BatchFailureStopsReconciliation(t *testing.T) {
	inst := &mockInstance{results: []callResult{{code: 1, stderr: "unknown model"}}}
	providers := map[string]GatewayProvider{"test-openai": {Key: "vk-test", APIType: "openai-completions"}}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test", []string{"test-openai/test-model"}, providers, 40001)
	if len(inst.calls) != 1 {
		t.Errorf("failed batch must not restart the gateway or partially reconcile: %v", inst.calls)
	}
	configBatchCall(t, inst.calls)
}

func TestConfigureInstance_ProviderOnlyBatch(t *testing.T) {
	inst := &mockInstance{}
	providers := map[string]GatewayProvider{"anthropic": {Key: "vk-test", APIType: "openai-completions"}}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test", nil, providers, 40001)
	batch := configBatchCall(t, inst.calls)
	if _, ok := configBatchValue(t, batch, "models.providers"); !ok {
		t.Errorf("provider-only batch missing models.providers: %v", batch)
	}
	for _, path := range []string{"agents.defaults.model", "agents.defaults.models", "agents.defaults.modelPolicy.allow"} {
		if _, ok := configBatchValue(t, batch, path); ok {
			t.Errorf("provider-only batch must not include %s", path)
		}
	}
}

func TestConfigureInstance_CustomProviderAllModels(t *testing.T) {
	// Custom providers (non-empty gp.Models) pass all models through regardless of effective list.
	inst := &mockInstance{}
	providers := map[string]GatewayProvider{
		"anthropic": {
			Key:     "vk-test",
			APIType: "anthropic-messages",
			Models: []database.ProviderModel{
				{ID: "anthropic/claude-opus-4-6", Name: "Claude Opus 4.6"},
				{ID: "anthropic/claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
			},
		},
	}
	// Effective list only contains sonnet, but custom providers ignore this — both models should appear.
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		[]string{"anthropic/anthropic/claude-sonnet-4-6"}, providers, 40001)

	batch := configBatchCall(t, inst.calls)
	providersValue, providersOK := configBatchValue(t, batch, "models.providers")
	allowlistValue, allowlistOK := configBatchValue(t, batch, "agents.defaults.models")
	providersJSON := string(providersValue)
	allowlistJSON := string(allowlistValue)
	if !providersOK || !allowlistOK {
		t.Fatalf("batch missing provider or allowlist state: %v", batch)
	}
	if providersJSON == "" {
		t.Fatal("models.providers call not found")
	}
	if !strings.Contains(providersJSON, "claude-opus-4-6") {
		t.Errorf("opus should be present (custom provider passes all models); got: %s", providersJSON)
	}
	if !strings.Contains(providersJSON, "claude-sonnet-4-6") {
		t.Errorf("sonnet should be present; got: %s", providersJSON)
	}
	// Models allowlist should only contain the effective model
	if allowlistJSON == "" {
		t.Fatal("agents.defaults.models call not found")
	}
	if !strings.Contains(allowlistJSON, "anthropic/anthropic/claude-sonnet-4-6") {
		t.Errorf("allowlist should contain the effective model; got: %s", allowlistJSON)
	}
}

func TestConfigureInstance_CatalogProviderModelsFiltered(t *testing.T) {
	// Catalog providers (empty gp.Models, CatalogKey set) use getCatalogModels + effectiveSet.
	orig := getCatalogModels
	getCatalogModels = func(catalogKey string) []database.ProviderModel {
		if catalogKey != "anthropic" {
			return nil
		}
		return []database.ProviderModel{
			{ID: "anthropic/claude-opus-4-6", Name: "Claude Opus 4.6"},
			{ID: "anthropic/claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
		}
	}
	defer func() { getCatalogModels = orig }()

	inst := &mockInstance{}
	providers := map[string]GatewayProvider{
		"anthropic": {Key: "vk-test", APIType: "anthropic-messages", CatalogKey: "anthropic"},
	}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		[]string{"anthropic/anthropic/claude-sonnet-4-6"}, providers, 40001)

	batch := configBatchCall(t, inst.calls)
	providersValue, providersOK := configBatchValue(t, batch, "models.providers")
	allowlistValue, allowlistOK := configBatchValue(t, batch, "agents.defaults.models")
	providersJSON := string(providersValue)
	allowlistJSON := string(allowlistValue)
	if !providersOK || !allowlistOK {
		t.Fatalf("batch missing provider or allowlist state: %v", batch)
	}
	if providersJSON == "" {
		t.Fatal("models.providers call not found")
	}
	if strings.Contains(providersJSON, "claude-opus-4-6") {
		t.Errorf("opus should be filtered out; got: %s", providersJSON)
	}
	if !strings.Contains(providersJSON, "claude-sonnet-4-6") {
		t.Errorf("sonnet should be present; got: %s", providersJSON)
	}
	// Models allowlist should match effective models
	if allowlistJSON == "" {
		t.Fatal("agents.defaults.models call not found")
	}
	if !strings.Contains(allowlistJSON, "anthropic/anthropic/claude-sonnet-4-6") {
		t.Errorf("allowlist should contain effective model; got: %s", allowlistJSON)
	}
}

func TestConfigureInstance_CatalogProviderWithCachedModelsFiltered(t *testing.T) {
	// Catalog provider with CatalogKey AND non-empty Models (cached) should still filter by effectiveSet.
	orig := getCatalogModels
	getCatalogModels = func(_ string) []database.ProviderModel {
		// Should not be called since Models is already populated.
		t.Error("getCatalogModels should not be called when Models is already cached")
		return nil
	}
	defer func() { getCatalogModels = orig }()

	inst := &mockInstance{}
	providers := map[string]GatewayProvider{
		"anthropic": {
			Key:        "vk-test",
			APIType:    "anthropic-messages",
			CatalogKey: "anthropic",
			Models: []database.ProviderModel{
				{ID: "anthropic/claude-opus-4-6", Name: "Claude Opus 4.6"},
				{ID: "anthropic/claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
			},
		},
	}
	// Effective list only contains sonnet.
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		[]string{"anthropic/anthropic/claude-sonnet-4-6"}, providers, 40001)

	batch := configBatchCall(t, inst.calls)
	providersValue, providersOK := configBatchValue(t, batch, "models.providers")
	allowlistValue, allowlistOK := configBatchValue(t, batch, "agents.defaults.models")
	providersJSON := string(providersValue)
	allowlistJSON := string(allowlistValue)
	if !providersOK || !allowlistOK {
		t.Fatalf("batch missing provider or allowlist state: %v", batch)
	}
	if providersJSON == "" {
		t.Fatal("models.providers call not found")
	}
	if strings.Contains(providersJSON, "claude-opus-4-6") {
		t.Errorf("opus should be filtered out even with cached models; got: %s", providersJSON)
	}
	if !strings.Contains(providersJSON, "claude-sonnet-4-6") {
		t.Errorf("sonnet should be present; got: %s", providersJSON)
	}
	// Models allowlist should match effective models
	if allowlistJSON == "" {
		t.Fatal("agents.defaults.models call not found")
	}
	if !strings.Contains(allowlistJSON, "anthropic/anthropic/claude-sonnet-4-6") {
		t.Errorf("allowlist should contain effective model; got: %s", allowlistJSON)
	}
}

func TestConfigureInstance_CatalogProviderFallsBackWhenNoneSelected(t *testing.T) {
	// Catalog provider with no models selected in the effective list must still
	// declare its full catalog. Declaring `models: []` gives OpenClaw a provider
	// it cannot route through, and the resulting config shrink trips OpenClaw's
	// size-drop write guard, which rejects the models.providers write outright
	// and leaves the agent with `"models": {}`.
	orig := getCatalogModels
	getCatalogModels = func(catalogKey string) []database.ProviderModel {
		return []database.ProviderModel{
			{ID: "anthropic/claude-opus-4-6", Name: "Claude Opus 4.6"},
		}
	}
	defer func() { getCatalogModels = orig }()

	inst := &mockInstance{}
	providers := map[string]GatewayProvider{
		"anthropic": {Key: "vk-test", APIType: "anthropic-messages", CatalogKey: "anthropic"},
	}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		nil, providers, 40001)

	batch := configBatchCall(t, inst.calls)
	providersValue, providersOK := configBatchValue(t, batch, "models.providers")
	providersJSON := string(providersValue)
	if !providersOK {
		t.Fatalf("batch missing models.providers: %v", batch)
	}
	if _, ok := configBatchValue(t, batch, "agents.defaults.models"); ok {
		t.Errorf("models allowlist should not be set when models is nil; got batch: %v", batch)
	}
	if providersJSON == "" {
		t.Fatal("models.providers call not found")
	}
	if !strings.Contains(providersJSON, "claude-opus-4-6") {
		t.Errorf("catalog models should be declared when none are selected; got: %s", providersJSON)
	}
	if strings.Contains(providersJSON, `"models":[]`) {
		t.Errorf("a provider must never be declared with an empty model list; got: %s", providersJSON)
	}
}

// TestConfigureInstance_NewProviderDoesNotEmptyConfig pins the regression that
// made an agent's `models` config go empty after a new provider was added.
//
// Two defects combined. buildOpenClawProvidersJSON filtered every catalog
// provider's model list down to the agent's selected models, so a provider
// nobody had picked models for yet -- the normal state right after adding one --
// was declared as `"models": []`. ConfigureInstance then wrote the map with
// `config unset models.providers` followed by `config set`. OpenClaw rejects
// both of those writes: `unset` is a >50% config shrink and trips the size-drop
// write guard, and a plain `set` on a protected map path is refused outright
// ("Refusing to replace models.providers; it would remove existing entries").
// When the unset did land and the set failed, the agent was left with
// `"models": {}` -- no providers at all -- and every later reconfigure repeated
// the same failing pair, so it never recovered.
func TestConfigureInstance_NewProviderDoesNotEmptyConfig(t *testing.T) {
	orig := getCatalogModels
	getCatalogModels = func(catalogKey string) []database.ProviderModel {
		return []database.ProviderModel{{ID: "gpt-5", Name: "GPT-5"}}
	}
	defer func() { getCatalogModels = orig }()

	inst := &mockInstance{}
	providers := map[string]GatewayProvider{
		// Already configured, one model selected on the agent.
		"anthropic": {
			Key: "vk-a", APIType: "anthropic-messages", CatalogKey: "anthropic",
			Models: []database.ProviderModel{
				{ID: "claude-sonnet-5", Name: "Claude Sonnet 5"},
				{ID: "claude-opus-5", Name: "Claude Opus 5"},
			},
		},
		// Just added; nothing selected for it yet.
		"openai": {Key: "vk-b", APIType: "openai-completions", CatalogKey: "openai"},
	}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		[]string{"anthropic/claude-sonnet-5"}, providers, 40001)

	batch := configBatchCall(t, inst.calls)
	if !containsArg(batch, "--replace") {
		t.Errorf("provider batch must pass --replace, got %v", batch)
	}
	providersValue, providersOK := configBatchValue(t, batch, "models.providers")
	providersJSON := string(providersValue)
	if !providersOK {
		t.Fatalf("batch missing models.providers: %v", batch)
	}
	if providersJSON == "" {
		t.Fatal("models.providers call not found")
	}
	if strings.Contains(providersJSON, `"models":[]`) {
		t.Fatalf("no provider may be declared with an empty model list; got: %s", providersJSON)
	}
	// The agent's explicit selection still wins for the provider it covers.
	if strings.Contains(providersJSON, "claude-opus-5") {
		t.Errorf("de-selected opus should not be declared; got: %s", providersJSON)
	}
	if !strings.Contains(providersJSON, "claude-sonnet-5") {
		t.Errorf("selected sonnet should be declared; got: %s", providersJSON)
	}
	// The brand-new provider falls back to its catalog instead of nothing.
	if !strings.Contains(providersJSON, "gpt-5") {
		t.Errorf("newly added provider should declare its catalog; got: %s", providersJSON)
	}
}

// --- per-model api adapter override (buildOpenClawProvidersJSON) ---

// decodeProviders unmarshals the models.providers JSON the builder emits.
func decodeProviders(t *testing.T, raw string) map[string]struct {
	API    string                   `json:"api"`
	Models []database.ProviderModel `json:"models"`
} {
	t.Helper()
	var got map[string]struct {
		API    string                   `json:"api"`
		Models []database.ProviderModel `json:"models"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal providers JSON: %v", err)
	}
	return got
}

// modelByID finds a declared model entry by id.
func modelByID(models []database.ProviderModel, id string) *database.ProviderModel {
	for i := range models {
		if models[i].ID == id {
			return &models[i]
		}
	}
	return nil
}

func TestBuildOpenClawProvidersJSON_PerModelAPIOverride(t *testing.T) {
	// OpenAI's native endpoint refuses function tools alongside reasoning_effort
	// on /v1/chat/completions, so reasoning models must be declared
	// openai-responses while non-reasoning models keep completions.
	providers := map[string]GatewayProvider{
		"openai": {
			Key:        "vk-test",
			APIType:    "openai-completions",
			BaseURL:    "https://api.openai.com/",
			CatalogKey: "openai",
			Models: []database.ProviderModel{
				{ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", Reasoning: true},
				{ID: "text-embedding-3-small", Name: "Embed Small"},
			},
		},
	}
	raw, err := buildOpenClawProvidersJSON(
		[]string{"openai/gpt-5.6-luna", "openai/text-embedding-3-small"}, providers, 40001)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := decodeProviders(t, raw)

	if got["openai"].API != "openai-completions" {
		t.Errorf("provider-level api should stay openai-completions, got %q", got["openai"].API)
	}
	if m := modelByID(got["openai"].Models, "gpt-5.6-luna"); m == nil {
		t.Fatal("gpt-5.6-luna missing from declaration")
	} else if m.API != "openai-responses" {
		t.Errorf("reasoning model should declare openai-responses, got %q", m.API)
	}
	if m := modelByID(got["openai"].Models, "text-embedding-3-small"); m == nil {
		t.Fatal("embedding model missing from declaration")
	} else if m.API != "" {
		t.Errorf("non-reasoning model should inherit provider api, got %q", m.API)
	}
}

func TestBuildOpenClawProvidersJSON_NoOverrideForThirdPartyEndpoint(t *testing.T) {
	// Third-party OpenAI-compatible endpoints commonly lack /v1/responses;
	// declaring it there would break them.
	cases := []struct {
		name       string
		baseURL    string
		catalogKey string
	}{
		{"non-openai catalog provider", "https://api.moonshot.ai/v1", "moonshot"},
		{"openai catalog key but proxied base URL", "https://litellm.internal/v1", "openai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			providers := map[string]GatewayProvider{
				"p": {
					Key: "vk-test", APIType: "openai-completions",
					BaseURL: tc.baseURL, CatalogKey: tc.catalogKey,
					Models: []database.ProviderModel{{ID: "m1", Name: "M1", Reasoning: true}},
				},
			}
			raw, err := buildOpenClawProvidersJSON([]string{"p/m1"}, providers, 40001)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if m := modelByID(decodeProviders(t, raw)["p"].Models, "m1"); m == nil {
				t.Fatal("m1 missing")
			} else if m.API != "" {
				t.Errorf("should not override api for %s, got %q", tc.baseURL, m.API)
			}
		})
	}
}

func TestBuildOpenClawProvidersJSON_ExplicitModelAPIWins(t *testing.T) {
	// An operator-set per-model api must survive inference, in both directions.
	providers := map[string]GatewayProvider{
		"openai": {
			Key: "vk-test", APIType: "openai-completions",
			BaseURL: "https://api.openai.com/", CatalogKey: "openai",
			Models: []database.ProviderModel{
				{ID: "pinned", Name: "Pinned", Reasoning: true, API: "openai-completions"},
			},
		},
	}
	raw, err := buildOpenClawProvidersJSON([]string{"openai/pinned"}, providers, 40001)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if m := modelByID(decodeProviders(t, raw)["openai"].Models, "pinned"); m == nil {
		t.Fatal("pinned missing")
	} else if m.API != "openai-completions" {
		t.Errorf("explicit per-model api should win, got %q", m.API)
	}
}

func TestBuildOpenClawProvidersJSON_AnthropicUnaffected(t *testing.T) {
	providers := map[string]GatewayProvider{
		"anthropic": {
			Key: "vk-test", APIType: "anthropic-messages",
			BaseURL: "https://api.anthropic.com/", CatalogKey: "anthropic",
			Models: []database.ProviderModel{{ID: "claude-sonnet-5", Name: "Sonnet 5", Reasoning: true}},
		},
	}
	raw, err := buildOpenClawProvidersJSON([]string{"anthropic/claude-sonnet-5"}, providers, 40001)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if m := modelByID(decodeProviders(t, raw)["anthropic"].Models, "claude-sonnet-5"); m == nil {
		t.Fatal("model missing")
	} else if m.API != "" {
		t.Errorf("anthropic models should not get an api override, got %q", m.API)
	}
}

func TestBuildOpenClawProvidersJSON_DoesNotMutateCallerModels(t *testing.T) {
	// The declaration is built on a copy; the caller's GatewayProvider.Models
	// (and any cached catalog slice behind it) must be left untouched.
	models := []database.ProviderModel{{ID: "gpt-5.6-luna", Name: "Luna", Reasoning: true}}
	providers := map[string]GatewayProvider{
		"openai": {
			Key: "vk-test", APIType: "openai-completions",
			BaseURL: "https://api.openai.com/", CatalogKey: "openai", Models: models,
		},
	}
	if _, err := buildOpenClawProvidersJSON([]string{"openai/gpt-5.6-luna"}, providers, 40001); err != nil {
		t.Fatalf("build: %v", err)
	}
	if models[0].API != "" {
		t.Errorf("caller's model slice was mutated: API=%q", models[0].API)
	}
}
