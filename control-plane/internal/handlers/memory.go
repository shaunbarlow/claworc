package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/middleware"
	"github.com/gluk-w/claworc/control-plane/internal/sshproxy"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
	"github.com/go-chi/chi/v5"
)

// memorySearchProviders are the embedding adapter ids OpenClaw's builtin
// memory engine ships with (see docs/reference/memory-config: "Provider
// selection"). "" means "leave memory.search.provider unset" (OpenClaw
// defaults to openai when an OpenAI key is configured, otherwise FTS-only).
// Also accepted: any custom `models.providers.<id>` key Claworc doesn't know
// about — validated as "non-empty string", not against this list, so a
// deliberate custom provider id (e.g. a self-hosted Ollama endpoint) always
// round-trips even though it isn't in the curated dropdown.
var memorySearchProviders = map[string]bool{
	"":                  true,
	"none":              true,
	"bedrock":           true,
	"deepinfra":         true,
	"gemini":            true,
	"github-copilot":    true,
	"lmstudio":          true,
	"local":             true,
	"mistral":           true,
	"ollama":            true,
	"openai":            true,
	"openai-compatible": true,
	"voyage":            true,
}

var memoryCitationsModes = map[string]bool{"": true, "auto": true, "on": true, "off": true}

// MemoryExtraPathEntry is one memory.search.extraPaths entry: either a bare
// path string (whole-directory/file index) or a {path, pattern} glob-scoped
// object. Renders to OpenClaw's own union shape verbatim.
type MemoryExtraPathEntry struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern,omitempty"`
}

// MemorySettings is the curated subset of OpenClaw's builtin memory.search.*
// config (plus top-level memory.citations) that Claworc manages with real
// form fields, plus a raw escape hatch for everything else. It is stored as
// JSON both in the default_memory_settings setting (global defaults) and in
// Instance.MemorySettings (per-instance override). Pointer/omitempty fields
// distinguish "not set — inherit" from an explicit value, mirroring
// LosslessClawSettings.
type MemorySettings struct {
	// Provider maps to memory.search.provider: embedding adapter id such as
	// "openai", "gemini", "local", or a custom models.providers.<id> key.
	// "" leaves it unset (OpenClaw auto-detects); "none" deliberately selects
	// FTS-only keyword search with no embeddings.
	Provider string `json:"provider,omitempty"`
	// Model maps to memory.search.model: embedding model name override.
	Model string `json:"model,omitempty"`
	// Fallback maps to memory.search.fallback: adapter id tried when the
	// primary provider fails. OpenClaw default is "none".
	Fallback string `json:"fallback,omitempty"`
	// MaxResults maps to memory.search.query.maxResults (OpenClaw default 6).
	MaxResults *int `json:"max_results,omitempty"`
	// MinScore maps to memory.search.query.minScore (0.0-1.0).
	MinScore *float64 `json:"min_score,omitempty"`
	// Citations maps to top-level memory.citations: "auto" | "on" | "off".
	Citations string `json:"citations,omitempty"`
	// RememberAcrossConversations maps to
	// memory.search.rememberAcrossConversations: let this agent recall
	// context from its own other recognized private conversations.
	RememberAcrossConversations *bool `json:"remember_across_conversations,omitempty"`
	// SessionsEnabled adds/removes "sessions" from memory.search.sources so
	// session transcripts are indexed and searchable, independent of
	// RememberAcrossConversations (which implies it but isn't required for
	// it). OpenClaw default sources is ["memory"] only.
	SessionsEnabled *bool `json:"sessions_enabled,omitempty"`
	// Advanced is a raw JSON object deep-merged into the generated
	// memory.search subtree last, so any OpenClaw option Claworc doesn't
	// model (multimodal, remote endpoint/headers, store.vector, cache,
	// input-type labels, ...) remains reachable.
	Advanced json.RawMessage `json:"advanced,omitempty"`
}

// memoryNonBlankRe matches any non-whitespace character; used to reject
// whitespace-only strings for free-text fields (provider/model/fallback).
var memoryNonBlankRe = regexp.MustCompile(`\S`)

// parseMemorySettings unmarshals and validates a MemorySettings JSON object.
// Unknown top-level keys are rejected so typos surface at save time instead
// of silently doing nothing (OpenClaw's own schema is strict too).
func parseMemorySettings(raw []byte) (MemorySettings, error) {
	var s MemorySettings
	if len(raw) == 0 || string(raw) == "null" {
		return s, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, err
	}
	if s.Provider != "" && !memorySearchProviders[s.Provider] {
		// Not a curated id — allow it through as a custom models.providers.*
		// reference (see field doc), but still reject obvious whitespace-only
		// junk so a stray space doesn't silently become an "unknown provider"
		// error deep in OpenClaw instead of here.
		if !memoryNonBlankRe.MatchString(s.Provider) {
			return s, fmt.Errorf("provider must not be blank")
		}
	}
	if s.Model != "" && !memoryNonBlankRe.MatchString(s.Model) {
		return s, fmt.Errorf("model must not be blank")
	}
	if s.Fallback != "" && !memorySearchProviders[s.Fallback] {
		if !memoryNonBlankRe.MatchString(s.Fallback) {
			return s, fmt.Errorf("fallback must not be blank")
		}
	}
	if s.MaxResults != nil && (*s.MaxResults < 1 || *s.MaxResults > 100) {
		return s, fmt.Errorf("max_results must be between 1 and 100")
	}
	if s.MinScore != nil && (*s.MinScore < 0 || *s.MinScore > 1) {
		return s, fmt.Errorf("min_score must be between 0 and 1")
	}
	if !memoryCitationsModes[s.Citations] {
		return s, fmt.Errorf("citations must be one of \"\", auto, on, off")
	}
	if len(s.Advanced) > 0 {
		var obj map[string]interface{}
		if err := json.Unmarshal(s.Advanced, &obj); err != nil {
			return s, fmt.Errorf("advanced must be a JSON object")
		}
	}
	return s, nil
}

// loadMemorySettings is the lenient variant used when reading stored values:
// bad JSON degrades to zero settings instead of erroring.
func loadMemorySettings(raw string) MemorySettings {
	var s MemorySettings
	if raw != "" {
		json.Unmarshal([]byte(raw), &s)
	}
	return s
}

// mergeMemorySettings overlays the per-instance override on the global
// defaults, field by field. Set fields win; unset fields inherit.
func mergeMemorySettings(global, override MemorySettings) MemorySettings {
	out := global
	if override.Provider != "" {
		out.Provider = override.Provider
	}
	if override.Model != "" {
		out.Model = override.Model
	}
	if override.Fallback != "" {
		out.Fallback = override.Fallback
	}
	if override.MaxResults != nil {
		out.MaxResults = override.MaxResults
	}
	if override.MinScore != nil {
		out.MinScore = override.MinScore
	}
	if override.Citations != "" {
		out.Citations = override.Citations
	}
	if override.RememberAcrossConversations != nil {
		out.RememberAcrossConversations = override.RememberAcrossConversations
	}
	if override.SessionsEnabled != nil {
		out.SessionsEnabled = override.SessionsEnabled
	}
	if len(override.Advanced) > 0 {
		out.Advanced = override.Advanced
	}
	return out
}

// memoryIndexedFolders returns the attached shared folders flagged for
// builtin-memory indexing, in stable order.
func memoryIndexedFolders(instanceID uint) []database.SharedFolder {
	folders, err := database.GetSharedFoldersForInstance(instanceID)
	if err != nil {
		return nil
	}
	out := make([]database.SharedFolder, 0, len(folders))
	for _, sf := range folders {
		if sf.MemoryIndex {
			out = append(out, sf)
		}
	}
	return out
}

// deepMergeJSON merges src into dst recursively; src values win, and nested
// objects merge key-wise. Used to overlay the Advanced escape hatch onto the
// generated memory.search subtree.
func deepMergeJSON(dst, src map[string]interface{}) map[string]interface{} {
	if dst == nil {
		dst = map[string]interface{}{}
	}
	for k, v := range src {
		if sv, ok := v.(map[string]interface{}); ok {
			if dv, ok := dst[k].(map[string]interface{}); ok {
				dst[k] = deepMergeJSON(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// buildMemoryConfig resolves the complete `memory` config subtree Claworc
// pushes into an instance's OpenClaw config: top-level `citations` plus the
// `search` subtree (provider/model/fallback, query limits,
// rememberAcrossConversations, sources, extraPaths from indexed shared
// folders, and the Advanced overlay). Returns an empty map when nothing is
// configured, matching "leave OpenClaw's own memory defaults alone".
//
// Claworc owns the memory.* subtree once this feature is in use: pushes
// replace it wholesale (one `config set … --replace`, see applyMemoryConfig)
// so removed folders and cleared overrides actually disappear.
func buildMemoryConfig(inst *database.Instance) map[string]interface{} {
	globalRaw, _ := database.GetSetting("default_memory_settings")
	s := mergeMemorySettings(loadMemorySettings(globalRaw), loadMemorySettings(inst.MemorySettings))

	search := map[string]interface{}{}
	if s.Provider != "" {
		search["provider"] = s.Provider
	}
	if s.Model != "" {
		search["model"] = s.Model
	}
	if s.Fallback != "" {
		search["fallback"] = s.Fallback
	}
	if s.MaxResults != nil || s.MinScore != nil {
		query := map[string]interface{}{}
		if s.MaxResults != nil {
			query["maxResults"] = *s.MaxResults
		}
		if s.MinScore != nil {
			query["minScore"] = *s.MinScore
		}
		search["query"] = query
	}
	if s.RememberAcrossConversations != nil {
		search["rememberAcrossConversations"] = *s.RememberAcrossConversations
	}
	if s.SessionsEnabled != nil {
		sources := []string{"memory"}
		if *s.SessionsEnabled {
			sources = []string{"memory", "sessions"}
		}
		search["sources"] = sources
	}

	if folders := memoryIndexedFolders(inst.ID); len(folders) > 0 {
		extraPaths := make([]interface{}, 0, len(folders))
		for i := range folders {
			if folders[i].MemoryIndexPattern != "" {
				extraPaths = append(extraPaths, map[string]interface{}{
					"path":    folders[i].MountPath,
					"pattern": folders[i].MemoryIndexPattern,
				})
			} else {
				extraPaths = append(extraPaths, folders[i].MountPath)
			}
		}
		search["extraPaths"] = extraPaths
	}

	if len(s.Advanced) > 0 {
		var adv map[string]interface{}
		if err := json.Unmarshal(s.Advanced, &adv); err == nil {
			search = deepMergeJSON(search, adv)
		}
	}

	cfg := map[string]interface{}{}
	if s.Citations != "" {
		cfg["citations"] = s.Citations
	}
	if len(search) > 0 {
		cfg["search"] = search
	}
	return cfg
}

// applyMemoryConfig replaces the memory subtree in the agent's OpenClaw
// config over an established SSH connection and restarts the gateway
// (top-level memory.* has no dedicated hot-reload rule in OpenClaw's
// config-reload planner, so it falls through to the default "restart"
// classification).
//
// One atomic write, same as applySlackConfig/applyDiscordConfig. `config set`
// replaces this subtree wholesale, so removed folders and cleared overrides
// disappear without an `unset` first — and unsetting is the worse option: it
// is a separate write that OpenClaw's size-drop guard rejects on a realistic
// config, and when it does land ahead of a failing set the agent loses its
// memory config entirely.
//
// Best-effort: failures are logged; the config is re-pushed on the next
// memory-affecting change.
func applyMemoryConfig(ctx context.Context, agent sshproxy.Instance, name string, cfg map[string]interface{}) {
	payload, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("memory-config: marshal for %s: %v", utils.SanitizeForLog(name), err)
		return
	}
	if _, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "set", "memory", string(payload), "--replace", "--json"); err != nil {
		log.Printf("memory-config: set memory for %s: %v", utils.SanitizeForLog(name), err)
		return
	} else if code != 0 {
		log.Printf("memory-config: set memory for %s failed: %s", utils.SanitizeForLog(name), utils.SanitizeForLog(stderr))
		return
	}
	// Restart the gateway to pick up the change (s6 restarts it after stop).
	if _, _, _, err := agent.ExecOpenclaw(ctx, "gateway", "stop"); err != nil {
		log.Printf("memory-config: gateway restart for %s: %v", utils.SanitizeForLog(name), err)
	}
}

// pushMemoryConfig asynchronously resolves and applies the memory config for
// an instance. The 120s SSH wait rides out a container restart (e.g. when a
// shared-folder membership change recreates the container before the new
// index paths can be pushed); openclaw.json lives on the home PVC, so a push
// that lands just before a restart still survives it.
func pushMemoryConfig(instanceID uint, name string) {
	if SSHMgr == nil {
		return
	}
	go func() {
		ctx := context.Background()
		sshClient, err := SSHMgr.WaitForSSH(ctx, instanceID, 120*time.Second)
		if err != nil {
			log.Printf("memory-config: no SSH connection for instance %d, skipping push: %v", instanceID, err)
			return
		}
		var inst database.Instance
		if err := database.DB.First(&inst, instanceID).Error; err != nil {
			return
		}
		applyMemoryConfig(ctx, sshproxy.NewSSHInstance(sshClient), name, buildMemoryConfig(&inst))
	}()
}

// pushMemoryConfigForFolder reconciles the OpenClaw memory config of every
// running, non-legacy instance in the given ID set. Used when a shared
// folder's memory-index flags, membership, or mount path change.
func pushMemoryConfigForFolder(instanceIDs []uint) {
	if len(instanceIDs) == 0 {
		return
	}
	var instances []database.Instance
	if err := database.DB.Where("id IN ?", instanceIDs).Find(&instances).Error; err != nil {
		return
	}
	for i := range instances {
		if database.IsLegacyEmbedded(instances[i].ContainerImage) {
			continue
		}
		if instances[i].Status != "running" {
			continue
		}
		pushMemoryConfig(instances[i].ID, instances[i].Name)
	}
}

// instanceMemoryResponse is the payload for GET/PATCH /instances/{id}/memory.
type instanceMemoryResponse struct {
	Settings               MemorySettings      `json:"settings"`           // per-instance override
	EffectiveSettings      MemorySettings      `json:"effective_settings"` // global defaults + override
	IndexedFolders         []indexedFolderResp `json:"indexed_folders"`
	RestartsGatewayOnApply bool                `json:"restarts_gateway_on_apply"`
}

type indexedFolderResp struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
	Pattern   string `json:"pattern"`
}

func buildInstanceMemoryResponse(inst *database.Instance) instanceMemoryResponse {
	globalRaw, _ := database.GetSetting("default_memory_settings")
	override := loadMemorySettings(inst.MemorySettings)

	folders := memoryIndexedFolders(inst.ID)
	indexed := make([]indexedFolderResp, 0, len(folders))
	for i := range folders {
		indexed = append(indexed, indexedFolderResp{
			ID:        folders[i].ID,
			Name:      folders[i].Name,
			MountPath: folders[i].MountPath,
			Pattern:   folders[i].MemoryIndexPattern,
		})
	}

	return instanceMemoryResponse{
		Settings:               override,
		EffectiveSettings:      mergeMemorySettings(loadMemorySettings(globalRaw), override),
		IndexedFolders:         indexed,
		RestartsGatewayOnApply: true,
	}
}

// GetInstanceMemory returns the instance's builtin memory search
// configuration: override, effective values, and the shared folders that
// feed its extra-paths index.
func GetInstanceMemory(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, buildInstanceMemoryResponse(&inst))
}

// SetInstanceMemory updates the per-instance memory settings override, then
// reconciles the agent's OpenClaw config (async, best-effort; requires a
// gateway restart to take effect).
func SetInstanceMemory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid instance ID")
		return
	}
	if !middleware.CanAccessInstance(r, uint(id)) {
		writeError(w, http.StatusForbidden, "Access denied")
		return
	}

	var body struct {
		Settings *json.RawMessage `json:"settings"` // full replacement of the override object
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if body.Settings == nil {
		writeError(w, http.StatusBadRequest, "No fields to update")
		return
	}

	var inst database.Instance
	if err := database.DB.First(&inst, uint(id)).Error; err != nil {
		writeError(w, http.StatusNotFound, "Instance not found")
		return
	}
	if database.IsLegacyEmbedded(inst.ContainerImage) {
		writeError(w, http.StatusBadRequest, "Memory configuration does not apply to legacy embedded instances")
		return
	}

	s, err := parseMemorySettings(*body.Settings)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid settings: "+err.Error())
		return
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to encode settings")
		return
	}

	if err := database.DB.Model(&inst).Update("memory_settings", string(encoded)).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update instance")
		return
	}
	inst.MemorySettings = string(encoded)

	// Reconcile the agent's OpenClaw config (async, best-effort).
	pushMemoryConfig(inst.ID, inst.Name)

	writeJSON(w, http.StatusOK, buildInstanceMemoryResponse(&inst))
}
