package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/middleware"
	"github.com/gluk-w/claworc/control-plane/internal/sshproxy"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
	"github.com/go-chi/chi/v5"
)

// contextEngineConfigSSHWait mirrors searchConfigSSHWait/pushMemoryConfig's
// wait budget: long enough to ride out a container restart triggered by the
// same settings/instance save that called this, since openclaw.json lives on
// the persisted home PVC and survives it.
const contextEngineConfigSSHWait = 120 * time.Second

// contextEnginePluginSpecs maps a managed context-engine selection to the npm
// package that provides it. lossless-claw ships as a separate package (see
// docs/concepts/context-engine.md's quick start), so selecting it requires an
// install step on the agent, not just a config write — same shape as
// searchProviderPluginSpecs in search.go.
//
// The engine id doubles as the OpenClaw plugin id today (both
// "lossless-claw"). A future engine whose plugin id differs from its
// registered contextEngine id would need a second map; not needed yet.
var contextEnginePluginSpecs = map[string]string{
	"lossless-claw": "@martian-engineering/lossless-claw",
}

// isValidContextEngine reports whether v is a context engine Claworc knows
// how to configure. "" means "inherit" (per-instance) or "use OpenClaw's own
// default" (global default) — both resolve to "legacy" downstream.
func isValidContextEngine(v string) bool {
	return v == "" || v == "legacy" || v == "lossless-claw"
}

// effectiveContextEngine resolves the context engine for an instance:
// per-instance override wins, otherwise the global default, otherwise
// "legacy" (OpenClaw's own built-in engine).
func effectiveContextEngine(inst *database.Instance) string {
	if inst.ContextEngine != "" {
		return inst.ContextEngine
	}
	if v, err := database.GetSetting("default_context_engine"); err == nil && isValidContextEngine(v) && v != "" {
		return v
	}
	return "legacy"
}

// LosslessClawFallbackProvider is one entry in lossless-claw's
// fallbackProviders list: an alternate provider/model pair the plugin may
// fall back to for its own summarization/expansion calls.
type LosslessClawFallbackProvider struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// LosslessClawSettings is the curated subset of lossless-claw's ~50-key
// config schema (confirmed live via `openclaw plugins inspect lossless-claw
// --json`) that Claworc manages with real form fields, plus a raw escape
// hatch for everything else. Stored as JSON both in the
// default_context_engine_settings setting (global defaults) and in
// Instance.ContextEngineSettings (per-instance override). Mirrors
// MemorySettings's pointer/omitempty pattern so "not set — inherit" is
// distinguishable from an explicit value.
type LosslessClawSettings struct {
	// ContextThreshold maps to contextThreshold: fraction of the context
	// window (0.0-1.0) that triggers compaction.
	ContextThreshold *float64 `json:"context_threshold,omitempty"`
	// FreshTailCount maps to freshTailCount: number of recent messages
	// protected from compaction.
	FreshTailCount *int `json:"fresh_tail_count,omitempty"`
	// LeafChunkTokens maps to leafChunkTokens: max source tokens per leaf
	// compaction chunk before summarization.
	LeafChunkTokens *int `json:"leaf_chunk_tokens,omitempty"`
	// SweepMaxDepth maps to sweepMaxDepth: preferred max condensation source
	// depth during routine sweeps (0 = leaf only, -1 = unlimited).
	SweepMaxDepth *int `json:"sweep_max_depth,omitempty"`
	// HostFallbackMode maps to hostFallbackMode: "error" | "capture-only".
	HostFallbackMode string `json:"host_fallback_mode,omitempty"`
	// PromptAwareEviction maps to promptAwareEviction: keep older context by
	// prompt relevance instead of pure chronology under budget pressure.
	PromptAwareEviction *bool `json:"prompt_aware_eviction,omitempty"`
	// StubLargeToolPayloads maps to stubLargeToolPayloads: replace evicted
	// large tool-result rows with an [LCM Tool Output: file_xxx] reference.
	StubLargeToolPayloads *bool `json:"stub_large_tool_payloads,omitempty"`
	// CustomInstructions maps to customInstructions: free-text guidance
	// appended to the plugin's own compaction/recall behavior.
	CustomInstructions string `json:"custom_instructions,omitempty"`
	// SummaryModel/SummaryProvider map to summaryModel/summaryProvider: the
	// model lossless-claw uses for its own summarization calls. Setting
	// either one requires Claworc to also grant
	// plugins.entries.lossless-claw.llm.allowModelOverride (see
	// buildContextEngineLLMPolicy) — OpenClaw ignores a plugin-requested
	// model override without it.
	SummaryModel    string `json:"summary_model,omitempty"`
	SummaryProvider string `json:"summary_provider,omitempty"`
	// FallbackProviders maps to fallbackProviders: alternate provider/model
	// pairs tried if the primary summary call fails.
	FallbackProviders []LosslessClawFallbackProvider `json:"fallback_providers,omitempty"`
	// Advanced is a raw JSON object deep-merged into the generated
	// plugins.entries.lossless-claw.config subtree last, so any of the
	// ~50 lossless-claw keys Claworc doesn't model (contextThresholdOverrides,
	// cacheAwareCompaction, dynamicLeafChunkTokens, autoRotateSessionFiles,
	// independentLogFile, timeouts, ...) remains reachable.
	Advanced json.RawMessage `json:"advanced,omitempty"`
}

// parseLosslessClawSettings unmarshals and validates a LosslessClawSettings
// JSON object. Unknown top-level keys are rejected so typos surface at save
// time instead of silently doing nothing, mirroring
// parseMemorySettings.
func parseLosslessClawSettings(raw []byte) (LosslessClawSettings, error) {
	var s LosslessClawSettings
	if len(raw) == 0 || string(raw) == "null" {
		return s, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, err
	}
	if s.ContextThreshold != nil && (*s.ContextThreshold < 0 || *s.ContextThreshold > 1) {
		return s, fmt.Errorf("context_threshold must be between 0 and 1")
	}
	if s.FreshTailCount != nil && *s.FreshTailCount < 1 {
		return s, fmt.Errorf("fresh_tail_count must be at least 1")
	}
	if s.LeafChunkTokens != nil && *s.LeafChunkTokens < 1 {
		return s, fmt.Errorf("leaf_chunk_tokens must be at least 1")
	}
	if s.SweepMaxDepth != nil && *s.SweepMaxDepth < -1 {
		return s, fmt.Errorf("sweep_max_depth must be -1 or greater")
	}
	switch s.HostFallbackMode {
	case "", "error", "capture-only":
	default:
		return s, fmt.Errorf("host_fallback_mode must be \"error\" or \"capture-only\"")
	}
	for i, fp := range s.FallbackProviders {
		if strings.TrimSpace(fp.Provider) == "" || strings.TrimSpace(fp.Model) == "" {
			return s, fmt.Errorf("fallback_providers[%d] needs both provider and model", i)
		}
	}
	if len(s.Advanced) > 0 {
		var obj map[string]interface{}
		if err := json.Unmarshal(s.Advanced, &obj); err != nil {
			return s, fmt.Errorf("advanced must be a JSON object")
		}
	}
	return s, nil
}

// loadLosslessClawSettings is the lenient variant used when reading stored
// values: bad JSON degrades to zero settings instead of erroring.
func loadLosslessClawSettings(raw string) LosslessClawSettings {
	var s LosslessClawSettings
	if raw != "" {
		json.Unmarshal([]byte(raw), &s)
	}
	return s
}

// mergeLosslessClawSettings overlays the per-instance override on the global
// defaults, field by field. Set fields win; unset fields inherit. Mirrors
// mergeMemorySettings.
func mergeLosslessClawSettings(global, override LosslessClawSettings) LosslessClawSettings {
	out := global
	if override.ContextThreshold != nil {
		out.ContextThreshold = override.ContextThreshold
	}
	if override.FreshTailCount != nil {
		out.FreshTailCount = override.FreshTailCount
	}
	if override.LeafChunkTokens != nil {
		out.LeafChunkTokens = override.LeafChunkTokens
	}
	if override.SweepMaxDepth != nil {
		out.SweepMaxDepth = override.SweepMaxDepth
	}
	if override.HostFallbackMode != "" {
		out.HostFallbackMode = override.HostFallbackMode
	}
	if override.PromptAwareEviction != nil {
		out.PromptAwareEviction = override.PromptAwareEviction
	}
	if override.StubLargeToolPayloads != nil {
		out.StubLargeToolPayloads = override.StubLargeToolPayloads
	}
	if override.CustomInstructions != "" {
		out.CustomInstructions = override.CustomInstructions
	}
	if override.SummaryModel != "" {
		out.SummaryModel = override.SummaryModel
	}
	if override.SummaryProvider != "" {
		out.SummaryProvider = override.SummaryProvider
	}
	if len(override.FallbackProviders) > 0 {
		out.FallbackProviders = override.FallbackProviders
	}
	if len(override.Advanced) > 0 {
		out.Advanced = override.Advanced
	}
	return out
}

// buildLosslessClawConfig renders the curated LosslessClawSettings plus the
// Advanced overlay into the plugins.entries.lossless-claw.config subtree
// OpenClaw expects.
func buildLosslessClawConfig(s LosslessClawSettings) map[string]interface{} {
	cfg := map[string]interface{}{}
	if s.ContextThreshold != nil {
		cfg["contextThreshold"] = *s.ContextThreshold
	}
	if s.FreshTailCount != nil {
		cfg["freshTailCount"] = *s.FreshTailCount
	}
	if s.LeafChunkTokens != nil {
		cfg["leafChunkTokens"] = *s.LeafChunkTokens
	}
	if s.SweepMaxDepth != nil {
		cfg["sweepMaxDepth"] = *s.SweepMaxDepth
	}
	if s.HostFallbackMode != "" {
		cfg["hostFallbackMode"] = s.HostFallbackMode
	}
	if s.PromptAwareEviction != nil {
		cfg["promptAwareEviction"] = *s.PromptAwareEviction
	}
	if s.StubLargeToolPayloads != nil {
		cfg["stubLargeToolPayloads"] = *s.StubLargeToolPayloads
	}
	if s.CustomInstructions != "" {
		cfg["customInstructions"] = s.CustomInstructions
	}
	if s.SummaryModel != "" {
		cfg["summaryModel"] = s.SummaryModel
	}
	if s.SummaryProvider != "" {
		cfg["summaryProvider"] = s.SummaryProvider
	}
	if len(s.FallbackProviders) > 0 {
		providers := make([]map[string]interface{}, 0, len(s.FallbackProviders))
		for _, fp := range s.FallbackProviders {
			providers = append(providers, map[string]interface{}{"provider": fp.Provider, "model": fp.Model})
		}
		cfg["fallbackProviders"] = providers
	}
	if len(s.Advanced) > 0 {
		var adv map[string]interface{}
		if err := json.Unmarshal(s.Advanced, &adv); err == nil {
			cfg = deepMergeJSON(cfg, adv)
		}
	}
	return cfg
}

// buildContextEngineLLMPolicy renders the
// plugins.entries.lossless-claw.llm policy subtree required for
// summaryModel/summaryProvider to actually take effect: OpenClaw ignores a
// plugin's requested model override in api.runtime.llm.complete unless
// allowModelOverride is explicitly granted, and allowedModels scopes which
// "provider/model" refs it may request (see gateway config.schema.lookup
// plugins.entries.*.llm). Returns ok=false when no override is configured, so
// the caller does not write an unnecessary trust grant.
func buildContextEngineLLMPolicy(s LosslessClawSettings) (policy map[string]interface{}, ok bool) {
	if s.SummaryModel == "" && s.SummaryProvider == "" {
		return nil, false
	}
	ref := s.SummaryProvider + "/" + s.SummaryModel
	if s.SummaryProvider == "" {
		ref = s.SummaryModel
	}
	return map[string]interface{}{
		"allowModelOverride": true,
		"allowedModels":      []string{ref},
	}, true
}

// contextEnginePluginPresent reports whether pluginID is discoverable in the
// decoded `plugins list --json` payload. known=false means the payload could
// not be parsed and the caller should not treat that as "missing". Mirrors
// searchPluginPresent.
func contextEnginePluginPresent(stdout, pluginID string) (present bool, known bool) {
	var listed openclawPluginsList
	if err := json.Unmarshal([]byte(extractJSONObject(stdout)), &listed); err != nil {
		return false, false
	}
	for _, p := range listed.Plugins {
		if p.ID == pluginID {
			return true, true
		}
	}
	return false, true
}

// ensureContextEnginePluginInstalled installs the plugin backing a managed
// context engine when the agent doesn't already have it. Runs inline (not
// fire-and-forget): applyContextEngineConfig is already off the request path,
// and the config it is about to push depends on the plugin being
// discoverable. Mirrors ensureSearchPluginInstalled.
//
// Only a confirmed-absent plugin is installed. Returns installed=true only
// when this call actually performed a fresh install, so the caller knows a
// gateway restart is needed to make the agent discover it.
func ensureContextEnginePluginInstalled(ctx context.Context, agent sshproxy.Instance, name, engine string) (installed bool) {
	spec, ok := contextEnginePluginSpecs[engine]
	if !ok {
		return false
	}
	stdout, _, code, err := agent.ExecOpenclaw(ctx, "plugins", "list", "--json")
	if err != nil || code != 0 {
		log.Printf("context-engine-config: %s: could not list installed plugins, skipping %s install check: %v", name, engine, err)
		return false
	}
	present, known := contextEnginePluginPresent(stdout, engine)
	if !known {
		log.Printf("context-engine-config: %s: could not read the agent's plugin list, skipping %s install check", name, engine)
		return false
	}
	if present {
		return false
	}
	log.Printf("context-engine-config: %s: installing %s for the %s context engine", name, spec, engine)
	_, stderr, code, err := agent.ExecOpenclaw(ctx, "plugins", "install", spec, "--acknowledge-clawhub-risk")
	if err != nil {
		log.Printf("context-engine-config: %s: %s install failed: %v", name, spec, err)
		return false
	}
	if code != 0 {
		log.Printf("context-engine-config: %s: %s install exited %d: %s", name, spec, code, utils.SanitizeForLog(stderr))
		return false
	}
	return true
}

// contextEngineConfigSet writes one config path on the agent and logs any
// failure. Mirrors searchConfigSet, including the --json-only-for-real-JSON
// caveat (a bare string with --json makes OpenClaw reject the whole write).
func contextEngineConfigSet(ctx context.Context, agent sshproxy.Instance, name, what, path, value string, extraArgs ...string) bool {
	args := append([]string{"config", "set", path, value}, extraArgs...)
	_, stderr, code, err := agent.ExecOpenclaw(ctx, args...)
	if err != nil {
		log.Printf("context-engine-config: %s: set %s: %v", name, what, err)
		return false
	}
	if code != 0 {
		log.Printf("context-engine-config: %s: set %s failed: %s", name, what, utils.SanitizeForLog(stderr))
		return false
	}
	return true
}

// contextEngineConfigUnset clears one Claworc-owned config path on the
// agent. Mirrors searchConfigUnset: an absent path counts as success.
func contextEngineConfigUnset(ctx context.Context, agent sshproxy.Instance, name, path string) bool {
	_, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "unset", path)
	if err != nil {
		log.Printf("context-engine-config: %s: unset %s: %v", name, path, err)
		return false
	}
	if code == 0 {
		return true
	}
	if strings.Contains(stderr, "Config path not found") {
		return true // never set on this agent; nothing to clear
	}
	log.Printf("context-engine-config: %s: unset %s failed: %s", name, path, utils.SanitizeForLog(stderr))
	return false
}

// applyContextEngineConfig reconciles the agent's context-engine config over
// SSH, in both directions: it writes Claworc's paths when a managed engine is
// selected and clears them when the selection goes back to "legacy".
//
// plugins.slots.contextEngine and plugins.entries.lossless-claw.* all report
// reloadKind "hot" (confirmed live via `gateway config.schema.lookup`), so an
// existing, already-installed plugin needs no gateway restart here — only a
// fresh install does, mirroring applySearchConfig (not applyMemoryConfig's
// always-restart pattern, which memory.* does not qualify for).
//
// Best-effort: failures are logged, same pattern as applySearchConfig.
func applyContextEngineConfig(ctx context.Context, agent sshproxy.Instance, name string, inst *database.Instance) {
	name = utils.SanitizeForLog(name)
	engine := effectiveContextEngine(inst)

	if engine != "lossless-claw" {
		// Back to legacy: drop the paths Claworc owns so an engine it
		// previously pinned stops being used. Per docs/concepts/context-engine.md,
		// uninstalling the selected plugin already resets the slot to
		// "legacy" on OpenClaw's side, but an explicit unset here also covers
		// an operator switching back without uninstalling.
		contextEngineConfigUnset(ctx, agent, name, "plugins.slots.contextEngine")
		for pluginID := range contextEnginePluginSpecs {
			contextEngineConfigUnset(ctx, agent, name, "plugins.entries."+pluginID+".enabled")
			contextEngineConfigUnset(ctx, agent, name, "plugins.entries."+pluginID+".llm")
			// Withdraw the conversation-access grant with the engine that
			// needed it. Only this leaf, so an operator's own hooks settings
			// (timeouts, allowPromptInjection) survive the switch back.
			contextEngineConfigUnset(ctx, agent, name, "plugins.entries."+pluginID+".hooks.allowConversationAccess")
		}
		return
	}

	globalRaw, _ := database.GetSetting("default_context_engine_settings")
	s := mergeLosslessClawSettings(loadLosslessClawSettings(globalRaw), loadLosslessClawSettings(inst.ContextEngineSettings))

	installedNow := ensureContextEnginePluginInstalled(ctx, agent, name, engine)

	cfg := buildLosslessClawConfig(s)
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("context-engine-config: %s: marshal %s config: %v", name, engine, err)
		return
	}
	if !contextEngineConfigSet(ctx, agent, name, engine+" plugin config",
		"plugins.entries."+engine+".config", string(cfgJSON), "--replace", "--json") {
		return
	}
	contextEngineConfigSet(ctx, agent, name, engine+" plugin enabled",
		"plugins.entries."+engine+".enabled", "true", "--json")

	// A context engine is useless without conversation access, and OpenClaw
	// denies it by default to every plugin that is not bundled with OpenClaw
	// itself. Enabling the engine without this leaves it loaded but inert, and
	// the only symptom is one line in the gateway log:
	//
	//   typed hook "before_prompt_build" blocked because non-bundled plugins
	//   must set plugins.entries.lossless-claw.hooks.allowConversationAccess=true
	//
	// so selecting the engine in Claworc has to carry the grant with it.
	// Written as a single leaf, not a `hooks` subtree replace: the rest of that
	// subtree (allowPromptInjection, timeoutMs, timeouts) is the operator's to
	// set by hand, and a --replace here would silently drop it.
	contextEngineConfigSet(ctx, agent, name, engine+" conversation access",
		"plugins.entries."+engine+".hooks.allowConversationAccess", "true", "--json")

	if policy, ok := buildContextEngineLLMPolicy(s); ok {
		policyJSON, err := json.Marshal(policy)
		if err == nil {
			contextEngineConfigSet(ctx, agent, name, engine+" llm policy",
				"plugins.entries."+engine+".llm", string(policyJSON), "--replace", "--json")
		}
	} else {
		contextEngineConfigUnset(ctx, agent, name, "plugins.entries."+engine+".llm")
	}

	// Plain string value — no --json (see contextEngineConfigSet).
	contextEngineConfigSet(ctx, agent, name, "plugins.slots.contextEngine",
		"plugins.slots.contextEngine", engine)

	if installedNow {
		if _, _, _, err := agent.ExecOpenclaw(ctx, "gateway", "stop"); err != nil {
			log.Printf("context-engine-config: %s: installed %s but could not restart the gateway: %v", name, engine, err)
		}
	}
}

// pushContextEngineConfig is the async best-effort wrapper around
// applyContextEngineConfig for a running instance, mirroring
// pushSearchConfig/pushMemoryConfig.
func pushContextEngineConfig(instanceID uint, name string) {
	if SSHMgr == nil {
		return
	}
	go func() {
		ctx := context.Background()
		sshClient, err := SSHMgr.WaitForSSH(ctx, instanceID, contextEngineConfigSSHWait)
		if err != nil {
			log.Printf("context-engine-config: no SSH connection for instance %d, skipping push: %v", instanceID, err)
			return
		}
		var inst database.Instance
		if err := database.DB.First(&inst, instanceID).Error; err != nil {
			return
		}
		applyContextEngineConfig(ctx, sshproxy.NewSSHInstance(sshClient), name, &inst)
	}()
}

// pushContextEngineConfigForRunningInstances reconciles the context-engine
// config of every running, non-legacy-embedded instance. Used when the
// global default changes (default_context_engine or
// default_context_engine_settings) so instances that inherit the global
// value pick it up without a per-instance edit. Mirrors
// pushSearchConfigForRunningInstances.
func pushContextEngineConfigForRunningInstances() {
	var running []database.Instance
	database.DB.Where("status = ?", "running").Find(&running)
	for i := range running {
		if database.IsLegacyEmbedded(running[i].ContainerImage) {
			continue
		}
		pushContextEngineConfig(running[i].ID, running[i].Name)
	}
}

// --- HTTP handlers -----------------------------------------------------

// instanceContextEngineResponse is the payload for
// GET/PATCH /instances/{id}/context-engine.
type instanceContextEngineResponse struct {
	ContextEngine          string               `json:"context_engine"` // "" = inherit
	EffectiveEngine        string               `json:"effective_engine"`
	DefaultEngine          string               `json:"default_engine"`
	LosslessClaw           LosslessClawSettings `json:"lossless_claw"`           // per-instance override
	EffectiveLosslessClaw  LosslessClawSettings `json:"effective_lossless_claw"` // global defaults + override
	RestartsGatewayOnApply bool                 `json:"restarts_gateway_on_apply"`
}

func buildInstanceContextEngineResponse(inst *database.Instance) instanceContextEngineResponse {
	defaultEngine := "legacy"
	if v, err := database.GetSetting("default_context_engine"); err == nil && isValidContextEngine(v) && v != "" {
		defaultEngine = v
	}
	globalRaw, _ := database.GetSetting("default_context_engine_settings")
	override := loadLosslessClawSettings(inst.ContextEngineSettings)

	return instanceContextEngineResponse{
		ContextEngine:          inst.ContextEngine,
		EffectiveEngine:        effectiveContextEngine(inst),
		DefaultEngine:          defaultEngine,
		LosslessClaw:           override,
		EffectiveLosslessClaw:  mergeLosslessClawSettings(loadLosslessClawSettings(globalRaw), override),
		RestartsGatewayOnApply: false,
	}
}

// GetInstanceContextEngine returns the instance's context-engine
// configuration: override, effective values, and the global default.
func GetInstanceContextEngine(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid instance ID")
		return
	}
	if !middleware.CanAccessInstance(r, uint(id)) {
		writeError(w, http.StatusForbidden, "Access denied")
		return
	}
	var inst database.Instance
	if err := database.DB.First(&inst, uint(id)).Error; err != nil {
		writeError(w, http.StatusNotFound, "Instance not found")
		return
	}
	writeJSON(w, http.StatusOK, buildInstanceContextEngineResponse(&inst))
}

// SetInstanceContextEngine updates the per-instance context-engine override
// and/or lossless-claw settings override, then reconciles the agent's
// OpenClaw config (async, best-effort; hot-reloads unless a fresh plugin
// install is needed).
func SetInstanceContextEngine(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid instance ID")
		return
	}
	if !middleware.CanMutateInstance(r, uint(id)) {
		writeError(w, http.StatusForbidden, "Access denied")
		return
	}

	var body struct {
		ContextEngine *string          `json:"context_engine"` // "" clears the override
		LosslessClaw  *json.RawMessage `json:"lossless_claw"`  // full replacement of the override object
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if body.ContextEngine == nil && body.LosslessClaw == nil {
		writeError(w, http.StatusBadRequest, "No fields to update")
		return
	}

	var inst database.Instance
	if err := database.DB.First(&inst, uint(id)).Error; err != nil {
		writeError(w, http.StatusNotFound, "Instance not found")
		return
	}
	if database.IsLegacyEmbedded(inst.ContainerImage) {
		writeError(w, http.StatusBadRequest, "Context engine configuration does not apply to legacy embedded instances")
		return
	}

	updates := map[string]interface{}{}
	if body.ContextEngine != nil {
		if !isValidContextEngine(*body.ContextEngine) {
			writeError(w, http.StatusBadRequest, "context_engine must be \"\", \"legacy\" or \"lossless-claw\"")
			return
		}
		updates["context_engine"] = *body.ContextEngine
	}
	if body.LosslessClaw != nil {
		s, err := parseLosslessClawSettings(*body.LosslessClaw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid lossless_claw settings: "+err.Error())
			return
		}
		encoded, err := json.Marshal(s)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to encode lossless_claw settings")
			return
		}
		updates["context_engine_settings"] = string(encoded)
	}

	if err := database.DB.Model(&inst).Updates(updates).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update instance")
		return
	}
	if v, ok := updates["context_engine"].(string); ok {
		inst.ContextEngine = v
	}
	if v, ok := updates["context_engine_settings"].(string); ok {
		inst.ContextEngineSettings = v
	}

	// Reconcile the agent's OpenClaw config (async, best-effort).
	pushContextEngineConfig(inst.ID, inst.Name)

	writeJSON(w, http.StatusOK, buildInstanceContextEngineResponse(&inst))
}
