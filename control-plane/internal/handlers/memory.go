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

// MemoryQmdSettings is the curated subset of OpenClaw's memory.qmd.* config
// that Claworc manages, plus a raw escape hatch for everything else. It is
// stored as JSON both in the default_memory_qmd setting (global defaults) and
// in Instance.MemoryQmd (per-instance override). Pointer/omitempty fields
// distinguish "not set — inherit" from an explicit value.
type MemoryQmdSettings struct {
	// SearchMode maps to memory.qmd.searchMode: "search" | "vsearch" | "query".
	SearchMode string `json:"search_mode,omitempty"`
	// UpdateInterval maps to memory.qmd.update.interval (e.g. "5m", "1h").
	UpdateInterval string `json:"update_interval,omitempty"`
	// MaxResults maps to memory.qmd.limits.maxResults.
	MaxResults *int `json:"max_results,omitempty"`
	// SessionsEnabled maps to memory.qmd.sessions.enabled (index transcripts).
	SessionsEnabled *bool `json:"sessions_enabled,omitempty"`
	// IncludeDefaultMemory maps to memory.qmd.includeDefaultMemory
	// (MEMORY.md / memory/**/*.md in the workspace).
	IncludeDefaultMemory *bool `json:"include_default_memory,omitempty"`
	// Advanced is a raw JSON object deep-merged into the generated
	// memory.qmd subtree last, so any OpenClaw option Claworc doesn't model
	// (scope rules, timeouts, debounce, ...) remains reachable.
	Advanced json.RawMessage `json:"advanced,omitempty"`
}

var memoryIntervalRe = regexp.MustCompile(`^\d+(ms|s|m|h)$`)

func isValidMemoryBackend(v string) bool {
	return v == "" || v == "builtin" || v == "qmd"
}

// parseMemoryQmdSettings unmarshals and validates a MemoryQmdSettings JSON
// object. Unknown top-level keys are rejected so typos surface at save time
// instead of silently doing nothing (OpenClaw's own schema is strict too).
func parseMemoryQmdSettings(raw []byte) (MemoryQmdSettings, error) {
	var s MemoryQmdSettings
	if len(raw) == 0 || string(raw) == "null" {
		return s, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, err
	}
	switch s.SearchMode {
	case "", "search", "vsearch", "query":
	default:
		return s, fmt.Errorf("search_mode must be one of search, vsearch, query")
	}
	if s.UpdateInterval != "" && !memoryIntervalRe.MatchString(s.UpdateInterval) {
		return s, fmt.Errorf("update_interval must look like \"30s\", \"5m\" or \"1h\"")
	}
	if s.MaxResults != nil && (*s.MaxResults < 1 || *s.MaxResults > 50) {
		return s, fmt.Errorf("max_results must be between 1 and 50")
	}
	if len(s.Advanced) > 0 {
		var obj map[string]interface{}
		if err := json.Unmarshal(s.Advanced, &obj); err != nil {
			return s, fmt.Errorf("advanced must be a JSON object")
		}
	}
	return s, nil
}

// loadMemoryQmdSettings is the lenient variant used when reading stored
// values: bad JSON degrades to zero settings instead of erroring.
func loadMemoryQmdSettings(raw string) MemoryQmdSettings {
	var s MemoryQmdSettings
	if raw != "" {
		json.Unmarshal([]byte(raw), &s)
	}
	return s
}

// mergeMemoryQmdSettings overlays the per-instance override on the global
// defaults, field by field. Set fields win; unset fields inherit.
func mergeMemoryQmdSettings(global, override MemoryQmdSettings) MemoryQmdSettings {
	out := global
	if override.SearchMode != "" {
		out.SearchMode = override.SearchMode
	}
	if override.UpdateInterval != "" {
		out.UpdateInterval = override.UpdateInterval
	}
	if override.MaxResults != nil {
		out.MaxResults = override.MaxResults
	}
	if override.SessionsEnabled != nil {
		out.SessionsEnabled = override.SessionsEnabled
	}
	if override.IncludeDefaultMemory != nil {
		out.IncludeDefaultMemory = override.IncludeDefaultMemory
	}
	if len(override.Advanced) > 0 {
		out.Advanced = override.Advanced
	}
	return out
}

// effectiveMemoryBackend resolves the memory backend for an instance:
// per-instance override, else global default, else "builtin".
func effectiveMemoryBackend(inst *database.Instance) string {
	if inst.MemoryBackend != "" {
		return inst.MemoryBackend
	}
	if v, err := database.GetSetting("default_memory_backend"); err == nil && (v == "builtin" || v == "qmd") {
		return v
	}
	return "builtin"
}

// qmdCollectionName derives a QMD collection slug from a shared folder name.
// OpenClaw suffixes collection names per agent, so this only needs to be
// stable and unique per folder — the folder ID guarantees that.
func qmdCollectionName(sf *database.SharedFolder) string {
	slug := strings.ToLower(sf.Name)
	slug = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return fmt.Sprintf("folder-%d", sf.ID)
	}
	return fmt.Sprintf("%s-%d", slug, sf.ID)
}

// qmdIndexedFolders returns the attached shared folders flagged for QMD
// indexing, in stable order.
func qmdIndexedFolders(instanceID uint) []database.SharedFolder {
	folders, err := database.GetSharedFoldersForInstance(instanceID)
	if err != nil {
		return nil
	}
	out := make([]database.SharedFolder, 0, len(folders))
	for _, sf := range folders {
		if sf.QmdIndex {
			out = append(out, sf)
		}
	}
	return out
}

// deepMergeJSON merges src into dst recursively; src values win, and nested
// objects merge key-wise. Used to overlay the Advanced escape hatch onto the
// generated memory.qmd subtree.
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
// pushes into an instance's OpenClaw config. For the builtin backend this is
// just {"backend":"builtin"}; for qmd it includes the merged knobs, the
// shared-folder index paths, and the Advanced overlay.
//
// Claworc owns the memory.* subtree once this feature is in use: pushes
// replace it wholesale (config unset + set) so removed folders and cleared
// overrides actually disappear despite `openclaw config set`'s deep-merge.
func buildMemoryConfig(inst *database.Instance) map[string]interface{} {
	backend := effectiveMemoryBackend(inst)
	cfg := map[string]interface{}{"backend": backend}
	if backend != "qmd" {
		return cfg
	}

	globalRaw, _ := database.GetSetting("default_memory_qmd")
	s := mergeMemoryQmdSettings(loadMemoryQmdSettings(globalRaw), loadMemoryQmdSettings(inst.MemoryQmd))

	qmd := map[string]interface{}{}
	if s.SearchMode != "" {
		qmd["searchMode"] = s.SearchMode
	}
	if s.UpdateInterval != "" {
		qmd["update"] = map[string]interface{}{"interval": s.UpdateInterval}
	}
	if s.MaxResults != nil {
		qmd["limits"] = map[string]interface{}{"maxResults": *s.MaxResults}
	}
	if s.SessionsEnabled != nil {
		qmd["sessions"] = map[string]interface{}{"enabled": *s.SessionsEnabled}
	}
	if s.IncludeDefaultMemory != nil {
		qmd["includeDefaultMemory"] = *s.IncludeDefaultMemory
	}

	if folders := qmdIndexedFolders(inst.ID); len(folders) > 0 {
		paths := make([]map[string]interface{}, 0, len(folders))
		for i := range folders {
			entry := map[string]interface{}{
				"path": folders[i].MountPath,
				"name": qmdCollectionName(&folders[i]),
			}
			if folders[i].QmdPattern != "" {
				entry["pattern"] = folders[i].QmdPattern
			}
			paths = append(paths, entry)
		}
		qmd["paths"] = paths
	}

	if len(s.Advanced) > 0 {
		var adv map[string]interface{}
		if err := json.Unmarshal(s.Advanced, &adv); err == nil {
			qmd = deepMergeJSON(qmd, adv)
		}
	}
	if len(qmd) > 0 {
		cfg["qmd"] = qmd
	}
	return cfg
}

// applyMemoryConfig replaces the memory subtree in the agent's OpenClaw
// config over an established SSH connection and restarts the gateway
// (memory.* is not hot-reloaded by OpenClaw). `config unset` first because
// `config set` deep-merges maps, which would leave removed keys behind.
// Best-effort: failures are logged; the config is re-pushed on the next
// memory-affecting change.
func applyMemoryConfig(ctx context.Context, agent sshproxy.Instance, name string, cfg map[string]interface{}) {
	payload, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("memory-config: marshal for %s: %v", utils.SanitizeForLog(name), err)
		return
	}
	if _, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "unset", "memory"); err != nil || code != 0 {
		// A missing key is fine; anything else is still only log-worthy.
		log.Printf("memory-config: unset memory for %s (code %d): %v %s",
			utils.SanitizeForLog(name), code, err, utils.SanitizeForLog(stderr))
	}
	if _, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "set", "memory", string(payload), "--json"); err != nil {
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
// folder's QMD flags, membership, or mount path change.
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
	MemoryBackend          string              `json:"memory_backend"` // "" = inherit
	EffectiveBackend       string              `json:"effective_backend"`
	DefaultBackend         string              `json:"default_backend"`
	Qmd                    MemoryQmdSettings   `json:"qmd"`           // per-instance override
	EffectiveQmd           MemoryQmdSettings   `json:"effective_qmd"` // global defaults + override
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
	defaultBackend := "builtin"
	if v, err := database.GetSetting("default_memory_backend"); err == nil && (v == "builtin" || v == "qmd") {
		defaultBackend = v
	}
	globalRaw, _ := database.GetSetting("default_memory_qmd")
	override := loadMemoryQmdSettings(inst.MemoryQmd)

	folders := qmdIndexedFolders(inst.ID)
	indexed := make([]indexedFolderResp, 0, len(folders))
	for i := range folders {
		pattern := folders[i].QmdPattern
		if pattern == "" {
			pattern = "**/*.md"
		}
		indexed = append(indexed, indexedFolderResp{
			ID:        folders[i].ID,
			Name:      folders[i].Name,
			MountPath: folders[i].MountPath,
			Pattern:   pattern,
		})
	}

	return instanceMemoryResponse{
		MemoryBackend:          inst.MemoryBackend,
		EffectiveBackend:       effectiveMemoryBackend(inst),
		DefaultBackend:         defaultBackend,
		Qmd:                    override,
		EffectiveQmd:           mergeMemoryQmdSettings(loadMemoryQmdSettings(globalRaw), override),
		IndexedFolders:         indexed,
		RestartsGatewayOnApply: true,
	}
}

// GetInstanceMemory returns the instance's memory backend configuration:
// override, effective values, and the shared folders that feed its QMD index.
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

// SetInstanceMemory updates the per-instance memory backend override and/or
// QMD settings override, then reconciles the agent's OpenClaw config
// (async, best-effort; requires a gateway restart to take effect).
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
		MemoryBackend *string          `json:"memory_backend"` // "" clears the override
		Qmd           *json.RawMessage `json:"qmd"`            // full replacement of the override object
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if body.MemoryBackend == nil && body.Qmd == nil {
		writeError(w, http.StatusBadRequest, "No fields to update")
		return
	}

	var inst database.Instance
	if err := database.DB.First(&inst, uint(id)).Error; err != nil {
		writeError(w, http.StatusNotFound, "Instance not found")
		return
	}
	if database.IsLegacyEmbedded(inst.ContainerImage) {
		writeError(w, http.StatusBadRequest, "Memory backend configuration does not apply to legacy embedded instances")
		return
	}

	updates := map[string]interface{}{}
	if body.MemoryBackend != nil {
		if !isValidMemoryBackend(*body.MemoryBackend) {
			writeError(w, http.StatusBadRequest, "memory_backend must be \"\", \"builtin\" or \"qmd\"")
			return
		}
		updates["memory_backend"] = *body.MemoryBackend
	}
	if body.Qmd != nil {
		s, err := parseMemoryQmdSettings(*body.Qmd)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid qmd settings: "+err.Error())
			return
		}
		encoded, err := json.Marshal(s)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to encode qmd settings")
			return
		}
		updates["memory_qmd"] = string(encoded)
	}

	if err := database.DB.Model(&inst).Updates(updates).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to update instance")
		return
	}
	if v, ok := updates["memory_backend"].(string); ok {
		inst.MemoryBackend = v
	}
	if v, ok := updates["memory_qmd"].(string); ok {
		inst.MemoryQmd = v
	}

	// Reconcile the agent's OpenClaw config (async, best-effort).
	pushMemoryConfig(inst.ID, inst.Name)

	writeJSON(w, http.StatusOK, buildInstanceMemoryResponse(&inst))
}
