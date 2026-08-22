package handlers

import (
	"context"
	"errors"
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
func (mockOps) DeleteSharedVolume(_ context.Context, _ uint) error               { return nil }
func (mockOps) CloneVolume(_ context.Context, _, _ string) error                 { return nil }
func (mockOps) VolumeNameFor(name, suffix string) string                         { return name + "-" + suffix }
func (mockOps) Apply(_ context.Context, _ orchestrator.WorkloadSpec) error       { return nil }
func (mockOps) DeleteWorkload(_ context.Context, _ orchestrator.WorkloadSpec) error {
	return nil
}
func (mockOps) EnsureSSHAccess(_ context.Context, _, _ string) error { return nil }
func (mockOps) WorkloadSSHAddress(_ context.Context, _ string) (string, int, error) {
	return "", 0, nil
}

func TestConfigureInstance_NoOp(t *testing.T) {
	inst := &mockInstance{}
	// Empty models and providers → early return, no calls
	ConfigureInstance(context.Background(), mockOps{}, inst, "test", nil, nil, 0)
	if len(inst.calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(inst.calls))
	}
}

func TestConfigureInstance_ModelSet(t *testing.T) {
	inst := &mockInstance{}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		[]string{"claude-3-5-sonnet"}, nil, 0)

	if len(inst.calls) < 3 {
		t.Fatalf("expected at least 3 calls (model set + allowlist set + gateway stop), got %d", len(inst.calls))
	}
	// First call: config set agents.defaults.model
	call0 := inst.calls[0]
	if call0[0] != "config" || call0[1] != "set" || call0[2] != "agents.defaults.model" {
		t.Errorf("unexpected first call: %v", call0)
	}
	// Second call: config set agents.defaults.models (allowlist). No preceding
	// `unset` -- see ConfigureInstance: unsetting is a separate write that
	// OpenClaw's size-drop guard rejects, and a failure after it lands leaves
	// the agent with no allowlist at all.
	call1 := inst.calls[1]
	if call1[0] != "config" || call1[1] != "set" || call1[2] != "agents.defaults.models" {
		t.Errorf("unexpected second call: %v", call1)
	}
	if !strings.Contains(call1[3], "claude-3-5-sonnet") {
		t.Errorf("models allowlist should contain claude-3-5-sonnet, got: %s", call1[3])
	}
	for _, c := range inst.calls {
		if c[1] == "unset" {
			t.Errorf("config unset must not be used; got %v", c)
		}
	}
	// The allowlist replaces de-selected models, which OpenClaw only permits
	// with --replace on this protected map path.
	if !containsArg(call1, "--replace") {
		t.Errorf("allowlist set must pass --replace, got %v", call1)
	}
	// Last call must be gateway stop
	last := inst.calls[len(inst.calls)-1]
	if last[0] != "gateway" || last[1] != "stop" {
		t.Errorf("expected last call to be gateway stop, got %v", last)
	}
}

func TestConfigureInstance_GatewayStop(t *testing.T) {
	inst := &mockInstance{}
	// Only providers → should set providers then stop gateway
	providers := map[string]GatewayProvider{
		"anthropic": {Key: "vk-test", APIType: "openai-completions"},
	}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		nil, providers, 40001)

	if len(inst.calls) < 1 {
		t.Fatalf("expected at least 1 call (gateway stop), got %d", len(inst.calls))
	}
	last := inst.calls[len(inst.calls)-1]
	if last[0] != "gateway" || last[1] != "stop" {
		t.Errorf("expected gateway stop, got %v", last)
	}
}

func TestConfigureInstance_ProvidersSet(t *testing.T) {
	inst := &mockInstance{}
	providers := map[string]GatewayProvider{
		"anthropic": {Key: "vk-test", APIType: "openai-completions"},
	}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		nil, providers, 40001)

	// Should have: providers set + gateway stop. One atomic write, no unset --
	// `unset models.providers` is rejected by OpenClaw's size-drop guard on any
	// realistic config, and when it does land a failing set leaves the agent
	// with `"models": {}`.
	if len(inst.calls) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(inst.calls))
	}
	if c := inst.calls[0]; c[0] != "config" || c[1] != "set" || c[2] != "models.providers" {
		t.Errorf("expected providers set first, got %v", c)
	}
	if !containsArg(inst.calls[0], "--replace") {
		t.Errorf("providers set must pass --replace, got %v", inst.calls[0])
	}
	for _, c := range inst.calls {
		if c[1] == "unset" {
			t.Errorf("config unset must not be used; got %v", c)
		}
	}
}

// containsArg reports whether an ExecOpenclaw arg list contains flag.
func containsArg(call []string, flag string) bool {
	for _, a := range call {
		if a == flag {
			return true
		}
	}
	return false
}

func TestConfigureInstance_NilModelsEmptySlice(t *testing.T) {
	inst := &mockInstance{}
	// Nil models but with gateway providers → skip model set and allowlist, set providers, stop gateway
	providers := map[string]GatewayProvider{
		"openai": {Key: "vk-test2", APIType: "openai-completions"},
	}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		nil, providers, 40001)

	for _, call := range inst.calls {
		if call[0] == "config" && call[2] == "agents.defaults.model" {
			t.Errorf("model set should not be called when models is nil, got call: %v", call)
		}
		if call[0] == "config" && call[2] == "agents.defaults.models" {
			t.Errorf("models allowlist should not be called when models is nil, got call: %v", call)
		}
	}
}

func TestConfigureInstance_ModelSetFailure(t *testing.T) {
	inst := &mockInstance{
		results: []callResult{
			{err: errors.New("SSH error")},
		},
	}
	// Should log error and return without calling gateway stop
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		[]string{"model-a"}, nil, 0)

	// Only one call was made (the failed one), gateway stop should not follow
	if len(inst.calls) != 1 {
		t.Errorf("expected 1 call (failed model set), got %d", len(inst.calls))
	}
}

func TestConfigureInstance_ModelSetNonZeroCode(t *testing.T) {
	inst := &mockInstance{
		results: []callResult{
			{code: 1, stderr: "unknown model"},
		},
	}
	providers := map[string]GatewayProvider{
		"anthropic": {Key: "vk-test", APIType: "openai-completions"},
	}
	ConfigureInstance(context.Background(), mockOps{}, inst, "test",
		[]string{"model-a"}, providers, 40001)

	hasProviders := false
	hasAllowlist := false
	for _, c := range inst.calls {
		if c[0] == "config" && c[1] == "set" && c[2] == "models.providers" {
			hasProviders = true
		}
		if c[0] == "config" && c[1] == "set" && c[2] == "agents.defaults.models" {
			hasAllowlist = true
		}
	}
	if !hasProviders {
		t.Errorf("providers must be set even when model config returns non-zero; calls: %v", inst.calls)
	}
	if !hasAllowlist {
		t.Errorf("models allowlist must be set even when model config returns non-zero; calls: %v", inst.calls)
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

	var providersJSON string
	var allowlistJSON string
	for _, c := range inst.calls {
		if c[0] == "config" && c[1] == "set" && c[2] == "models.providers" {
			providersJSON = c[3]
		}
		if c[0] == "config" && c[1] == "set" && c[2] == "agents.defaults.models" {
			allowlistJSON = c[3]
		}
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

	var providersJSON string
	var allowlistJSON string
	for _, c := range inst.calls {
		if c[0] == "config" && c[1] == "set" && c[2] == "models.providers" {
			providersJSON = c[3]
		}
		if c[0] == "config" && c[1] == "set" && c[2] == "agents.defaults.models" {
			allowlistJSON = c[3]
		}
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

	var providersJSON string
	var allowlistJSON string
	for _, c := range inst.calls {
		if c[0] == "config" && c[1] == "set" && c[2] == "models.providers" {
			providersJSON = c[3]
		}
		if c[0] == "config" && c[1] == "set" && c[2] == "agents.defaults.models" {
			allowlistJSON = c[3]
		}
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

	var providersJSON string
	for _, c := range inst.calls {
		if c[0] == "config" && c[1] == "set" && c[2] == "models.providers" {
			providersJSON = c[3]
		}
		if c[0] == "config" && c[1] == "set" && c[2] == "agents.defaults.models" {
			t.Errorf("models allowlist should not be set when models is nil; got call: %v", c)
		}
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

	var providersJSON string
	for _, c := range inst.calls {
		if c[1] == "unset" {
			t.Errorf("config unset must not be used -- OpenClaw rejects the shrink; got %v", c)
		}
		if c[0] == "config" && c[1] == "set" && c[2] == "models.providers" {
			providersJSON = c[3]
			if !containsArg(c, "--replace") {
				t.Errorf("providers set must pass --replace, got %v", c)
			}
		}
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
