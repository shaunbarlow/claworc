package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gluk-w/claworc/control-plane/internal/analytics"
	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

// fixedEncryptedSettings are non-LLM keys stored as fixed setting entries.
var fixedEncryptedSettings = map[string]bool{
	"brave_api_key":            true,
	"connector_encryption_key": true,
	"connector_admin_token":    true,
	"openbao_root_token":       true,
	"openbao_unseal_key":       true,
	"openbao_admin_token":      true,
}

// plainSettings are returned as-is (not encrypted).
var plainSettings = []string{
	"default_container_image",
	"default_agent_image",
	"default_browser_image",
	"default_browser_provider",
	"default_browser_idle_minutes",
	"default_browser_ready_seconds",
	"default_browser_storage",
	"default_vnc_resolution",
	"default_cpu_request",
	"default_cpu_limit",
	"default_memory_request",
	"default_memory_limit",
	"default_storage_homebrew",
	"default_storage_home",
	"default_timezone",
	"default_user_agent",
	"default_models",
	"default_search_provider",
	"default_context_engine",
	"analytics_consent",
	"connector_enabled",
	"connector_image",
	"connector_storage",
	"connector_origin",
	"connector_allowed_custom_oauth",
	"openbao_enabled",
	"openbao_image",
	"openbao_storage",
}

func getAllSettings() map[string]string {
	var settings []database.Setting
	database.DB.Find(&settings)
	result := make(map[string]string)
	for _, s := range settings {
		result[s.Key] = s.Value
	}
	return result
}

func settingsToResponse(raw map[string]string) map[string]interface{} {
	result := make(map[string]interface{})

	// Plain settings
	for _, key := range plainSettings {
		if key == "default_models" {
			var models []string
			if err := json.Unmarshal([]byte(raw[key]), &models); err != nil || raw[key] == "" {
				models = []string{}
			}
			result[key] = models
			continue
		}
		if key == "analytics_consent" {
			v := raw[key]
			if v == "" {
				v = analytics.ConsentUnset
			}
			result[key] = v
			continue
		}
		result[key] = raw[key]
	}

	// Read-only: surface installation_id (auto-generated on first GET) so the
	// settings UI can show the user the random ID we report alongside events.
	id, _ := analytics.GetOrCreateInstallationID()
	result["installation_id"] = id

	// Fixed encrypted settings (brave_api_key)
	for key := range fixedEncryptedSettings {
		val := raw[key]
		if val != "" {
			decrypted, err := utils.Decrypt(val)
			if err != nil {
				result[key] = ""
			} else {
				result[key] = utils.Mask(decrypted)
			}
		} else {
			result[key] = ""
		}
	}

	// Global env vars — decrypt and surface as plaintext. Settings is an
	// admin-only surface; masking offers no real confidentiality here and the
	// edit flow needs the live value to diff against.
	result["default_env_vars"] = EnvVarsForResponse(raw["default_env_vars"])

	// Named OpenBao shared secret sets (admin-managed; see openbao_shared_sets.go).
	result["openbao_shared_sets"] = listSharedSecretSetNames()

	// Global pod placement settings
	for _, k := range []string{"default_pod_annotations", "default_node_selector", "default_service_account_annotations"} {
		var m map[string]string
		if raw[k] != "" {
			json.Unmarshal([]byte(raw[k]), &m)
		}
		if m == nil {
			m = map[string]string{}
		}
		result[k] = m
	}
	for _, k := range []string{"default_tolerations", "default_ports"} {
		var list []interface{}
		if raw[k] != "" {
			json.Unmarshal([]byte(raw[k]), &list)
		}
		if list == nil {
			list = []interface{}{}
		}
		result[k] = list
	}
	result["default_affinity"] = raw["default_affinity"]

	// Global builtin memory defaults (JSON MemorySettings object).
	result["default_memory_settings"] = loadMemorySettings(raw["default_memory_settings"])

	// Global lossless-claw context-engine defaults (JSON LosslessClawSettings object).
	result["default_context_engine_settings"] = loadLosslessClawSettings(raw["default_context_engine_settings"])

	return result
}

func GetSettings(w http.ResponseWriter, r *http.Request) {
	raw := getAllSettings()
	writeJSON(w, http.StatusOK, settingsToResponse(raw))
}

type settingsUpdateRequest struct {
	DefaultModels *json.RawMessage       `json:"default_models,omitempty"`
	BraveAPIKey   *string                `json:"brave_api_key,omitempty"`
	Plain         map[string]interface{} `json:"-"` // remaining plain fields
}

func UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var raw map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	resourceFromRaw := func(key string) string {
		if v, ok := raw[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	if err := ValidateResourceQuantities(ResourceQuantities{
		CPURequest:      resourceFromRaw("default_cpu_request"),
		CPULimit:        resourceFromRaw("default_cpu_limit"),
		MemoryRequest:   resourceFromRaw("default_memory_request"),
		MemoryLimit:     resourceFromRaw("default_memory_limit"),
		StorageHome:     resourceFromRaw("default_storage_home"),
		StorageHomebrew: resourceFromRaw("default_storage_homebrew"),
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Handle default_models
	if v, ok := raw["default_models"]; ok {
		b, err := json.Marshal(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid default_models")
			return
		}
		database.SetSetting("default_models", string(b))
	}

	// Handle brave_api_key (fixed encrypted). Tracked separately from
	// searchChanged below since changing the key matters even when the
	// provider selection itself doesn't (e.g. rotating a key for an instance
	// that already has "brave" pinned).
	searchChanged := false
	if v, ok := raw["brave_api_key"]; ok {
		if strVal, ok := v.(string); ok {
			searchChanged = true
			if strVal != "" {
				encrypted, err := utils.Encrypt(strVal)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "Failed to encrypt API key")
					return
				}
				database.SetSetting("brave_api_key", encrypted)
			} else {
				database.SetSetting("brave_api_key", "")
			}
		}
	}

	// Handle default search provider. "" means leave OpenClaw's own
	// auto-detection alone, which is a valid, explicit choice here.
	if v, ok := raw["default_search_provider"]; ok {
		if strVal, ok := v.(string); ok {
			if !isValidSearchProvider(strVal) {
				writeError(w, http.StatusBadRequest, "default_search_provider must be \"\" or \"brave\"")
				return
			}
			prev, _ := database.GetSetting("default_search_provider")
			if strVal != prev {
				searchChanged = true
			}
		}
	}

	// Handle the managed OpenConnector deployment. connector_enabled is the
	// only field that triggers a lifecycle action (Apply/Stop); image/storage/
	// origin are read fresh out of the database by resolvedConnectorConfig on
	// every apply, so editing them alone (without touching connector_enabled)
	// falls through to the generic plain-settings loop below and takes effect
	// on the *next* apply rather than immediately -- matching how
	// default_container_image only affects instances created/restarted after
	// the edit, not already-running ones.
	if v, ok := raw["connector_enabled"]; ok {
		strVal, isString := v.(string)
		boolVal, isBool := v.(bool)
		var enabled bool
		switch {
		case isString:
			if strVal != "true" && strVal != "false" {
				writeError(w, http.StatusBadRequest, "connector_enabled must be true or false")
				return
			}
			enabled = strVal == "true"
		case isBool:
			enabled = boolVal
		default:
			writeError(w, http.StatusBadRequest, "connector_enabled must be true or false")
			return
		}
		prev, _ := database.GetSetting("connector_enabled")
		prevEnabled := prev == "true"
		if enabled {
			database.SetSetting("connector_enabled", "true")
			if _, err := ensureConnectorSecrets(); err != nil {
				writeError(w, http.StatusInternalServerError, "Failed to generate connector secrets: "+err.Error())
				return
			}
		} else {
			database.SetSetting("connector_enabled", "false")
		}
		if enabled != prevEnabled {
			applyConnectorAsync(enabled)
		}
	}

	// Handle the managed OpenBao deployment. Same shape as connector_enabled
	// above -- openbao_enabled is the only field with an immediate lifecycle
	// action; openbao_image/openbao_storage are read fresh on every apply.
	if v, ok := raw["openbao_enabled"]; ok {
		strVal, isString := v.(string)
		boolVal, isBool := v.(bool)
		var enabled bool
		switch {
		case isString:
			if strVal != "true" && strVal != "false" {
				writeError(w, http.StatusBadRequest, "openbao_enabled must be true or false")
				return
			}
			enabled = strVal == "true"
		case isBool:
			enabled = boolVal
		default:
			writeError(w, http.StatusBadRequest, "openbao_enabled must be true or false")
			return
		}
		prev, _ := database.GetSetting("openbao_enabled")
		prevEnabled := prev == "true"
		if enabled {
			database.SetSetting("openbao_enabled", "true")
		} else {
			database.SetSetting("openbao_enabled", "false")
		}
		if enabled != prevEnabled {
			applyOpenbaoAsync(enabled)
		}
	}

	// Handle env_vars_set / env_vars_unset (PATCH-style for the encrypted map).
	// envVarsChanged is true only when the resulting plaintext map actually
	// differs from what was stored — a no-op request (e.g. re-setting the same
	// value, or an empty set/unset pair) skips the save and skips the restart
	// that would otherwise cascade to every running instance.
	envSet, envUnset, err := parseEnvVarsDelta(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	envVarsChanged := false
	if len(envSet) > 0 || len(envUnset) > 0 {
		existing, _ := database.GetSetting("default_env_vars")
		updated, changed, err := ApplyEnvVarsDelta(existing, envSet, envUnset)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to update env vars: "+err.Error())
			return
		}
		if changed {
			if err := database.SetSetting("default_env_vars", updated); err != nil {
				writeError(w, http.StatusInternalServerError, "Failed to save env vars")
				return
			}
			envVarsChanged = true
			analytics.Track(r.Context(), analytics.EventGlobalEnvVarsEdited, map[string]any{
				"total_env_vars": len(decodeEncryptedEnvVarsJSON(updated)),
			})
		}
	}

	// Handle default context engine. "" is accepted and resolves to "legacy"
	// downstream (see effectiveContextEngine), matching default_search_provider's
	// "empty means leave it alone" treatment.
	contextEngineChanged := false
	if v, ok := raw["default_context_engine"]; ok {
		if strVal, ok := v.(string); ok {
			if !isValidContextEngine(strVal) {
				writeError(w, http.StatusBadRequest, "default_context_engine must be \"\", \"legacy\" or \"lossless-claw\"")
				return
			}
			prev, _ := database.GetSetting("default_context_engine")
			if strVal != prev {
				contextEngineChanged = true
			}
		}
	}
	if v, ok := raw["default_context_engine_settings"]; ok {
		b, err := json.Marshal(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid default_context_engine_settings")
			return
		}
		if _, err := parseLosslessClawSettings(b); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid default_context_engine_settings: "+err.Error())
			return
		}
		prev, _ := database.GetSetting("default_context_engine_settings")
		if string(b) != prev {
			contextEngineChanged = true
		}
		database.SetSetting("default_context_engine_settings", string(b))
	}

	// Handle builtin memory defaults. Track whether the key actually changed
	// so we can reconcile running instances' OpenClaw config below.
	memoryChanged := false
	if v, ok := raw["default_memory_settings"]; ok {
		b, err := json.Marshal(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid default_memory_settings")
			return
		}
		if _, err := parseMemorySettings(b); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid default_memory_settings: "+err.Error())
			return
		}
		prev, _ := database.GetSetting("default_memory_settings")
		if string(b) != prev {
			memoryChanged = true
		}
		database.SetSetting("default_memory_settings", string(b))
	}

	// Handle pod placement + service/port settings (stored as JSON strings)
	for _, key := range []string{
		"default_pod_annotations", "default_node_selector", "default_tolerations",
		"default_service_account_annotations", "default_ports",
	} {
		if v, ok := raw[key]; ok {
			b, err := json.Marshal(v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "Invalid "+key)
				return
			}
			database.SetSetting(key, string(b))
		}
	}

	// Handle remaining plain settings
	for key, val := range raw {
		if key == "default_models" || key == "brave_api_key" || key == "env_vars_set" || key == "env_vars_unset" {
			continue
		}
		if key == "connector_enabled" {
			continue // handled above (secrets generation + Apply/Stop trigger)
		}
		if key == "connector_encryption_key" || key == "connector_admin_token" {
			// Generated internally (ensureConnectorSecrets); never accepted as a
			// direct plaintext write — fixedEncryptedSettings expects an
			// already-encrypted value in the DB, and the generic loop below would
			// otherwise happily store the caller's raw string as-is.
			continue
		}
		if key == "default_pod_annotations" || key == "default_node_selector" || key == "default_tolerations" ||
			key == "default_service_account_annotations" || key == "default_ports" {
			continue // handled above
		}
		// installation_id is read-only; never accept it on update.
		if key == "installation_id" {
			continue
		}
		if strVal, ok := val.(string); ok {
			if key == "analytics_consent" {
				if strVal != analytics.ConsentOptIn && strVal != analytics.ConsentOptOut {
					// Reject "unset" or unknown values — once shown the modal,
					// users can only flip between in and out.
					continue
				}
				prev := analytics.GetConsent()
				// Send the opt_out event BEFORE persisting so Track()'s consent
				// gate doesn't short-circuit it. Then store the new state.
				if strVal == analytics.ConsentOptOut && prev == analytics.ConsentOptIn {
					analytics.TrackForceOptOut()
				}
				database.SetSetting(key, strVal)
				continue
			}
			database.SetSetting(key, strVal)
		}
	}

	// Auto-restart every instance whose container is missing the new global
	// env vars. The container only injects env vars on (re)create, so without
	// this the DB and the live containers silently diverge.
	//
	// Deliberately not filtered by `status = 'running'`: the status column
	// lags reality, and an instance whose row is stale is exactly the one that
	// missed a previous cascade and most needs this one. EnsureEnvPropagated
	// checks the live container itself, skips anything that is not actually
	// up, and restarts only on real drift -- so an instance that already has
	// the values (per-instance override, or caught by an earlier pass) is left
	// alone rather than needlessly bounced.
	var restartingInstances []restartTarget
	if envVarsChanged {
		touched := make([]string, 0, len(envSet)+len(envUnset))
		for name := range envSet {
			touched = append(touched, name)
		}
		touched = append(touched, envUnset...)

		var instances []database.Instance
		database.DB.Find(&instances)
		for i := range instances {
			if !EnsureEnvPropagated(r.Context(), instances[i], callerID(r), touched...) {
				continue
			}
			restartingInstances = append(restartingInstances, restartTarget{
				ID:          instances[i].ID,
				Name:        instances[i].Name,
				DisplayName: instances[i].DisplayName,
			})
		}
	}

	// Reconcile every running instance's OpenClaw memory config when the
	// global memory defaults changed. Much cheaper than the env-var cascade
	// above: only the openclaw-gateway process restarts, not the container.
	// Instances with a full per-instance override still get a push — the
	// resolved config is computed per instance, so theirs simply re-applies
	// the same values.
	if memoryChanged {
		var running []database.Instance
		database.DB.Where("status = ?", "running").Find(&running)
		for i := range running {
			if database.IsLegacyEmbedded(running[i].ContainerImage) {
				continue
			}
			pushMemoryConfig(running[i].ID, running[i].Name)
		}
	}

	// Reconcile every instance's search-provider setup when the global
	// default provider or the global Brave key changed. Two independent
	// concerns, same trigger:
	//   - BRAVE_API_KEY only reaches a container on (re)create, so instances
	//     that inherit the global key (no per-instance override) need the
	//     same drift-check restart path as the generic env-var cascade above.
	//   - tools.web.search.provider / plugins.entries.brave.config are
	//     hot-reloadable, so a config push over SSH is enough — no restart
	//     needed unless a fresh plugin install forces one (applySearchConfig
	//     handles that itself).
	// Checked against every instance (not just running ones) for the env
	// part, matching the envVarsChanged cascade's reasoning; the config push
	// stays scoped to running instances since a stopped agent has nothing to
	// push to.
	if searchChanged {
		var instances []database.Instance
		database.DB.Find(&instances)
		for i := range instances {
			// Every instance is considered, not just the ones resolving to
			// Brave. An instance that just stopped resolving to Brave is
			// precisely the one that needs work: its BRAVE_API_KEY has to come
			// back out of the container env, and the tools.web.search.provider
			// Claworc pinned has to be unset again (applySearchConfig's
			// teardown branch). Skipping it left the agent pinned to a
			// provider the operator had already turned off.
			if EnsureEnvPropagated(r.Context(), instances[i], callerID(r), "BRAVE_API_KEY") {
				restartingInstances = append(restartingInstances, restartTarget{
					ID:          instances[i].ID,
					Name:        instances[i].Name,
					DisplayName: instances[i].DisplayName,
				})
			}
			// The config push is needed whether or not a restart was started
			// (see the same fix in UpdateInstance): the env var and the agent's
			// config are separate channels, and only the push writes the
			// provider selection. Still scoped to running instances — a stopped
			// agent has no SSH to push over, and picks the config up from the
			// next save once it is back.
			if instances[i].Status == "running" && !database.IsLegacyEmbedded(instances[i].ContainerImage) {
				pushSearchConfig(instances[i].ID, instances[i].Name)
			}
		}
	}

	// Reconcile every running instance's OpenClaw context-engine config when
	// the global defaults changed. Hot-reloadable (see applyContextEngineConfig),
	// so — unlike the memory/search cascades above — this never needs the
	// env-var drift-restart path, only a config push.
	if contextEngineChanged {
		pushContextEngineConfigForRunningInstances()
	}

	resp := settingsToResponse(getAllSettings())
	if len(restartingInstances) > 0 {
		resp["restarting_instances"] = restartingInstances
	}
	writeJSON(w, http.StatusOK, resp)
}

type restartTarget struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// parseEnvVarsDelta extracts env_vars_set (map[string]string) and
// env_vars_unset ([]string) from the raw JSON body. Missing keys → empty
// results. Malformed values → error.
func parseEnvVarsDelta(raw map[string]interface{}) (map[string]string, []string, error) {
	set := map[string]string{}
	if v, ok := raw["env_vars_set"]; ok && v != nil {
		m, ok := v.(map[string]interface{})
		if !ok {
			return nil, nil, fmt.Errorf("env_vars_set must be an object")
		}
		for k, val := range m {
			s, ok := val.(string)
			if !ok {
				return nil, nil, fmt.Errorf("env_vars_set[%s] must be a string", k)
			}
			set[k] = s
		}
	}
	var unset []string
	if v, ok := raw["env_vars_unset"]; ok && v != nil {
		arr, ok := v.([]interface{})
		if !ok {
			return nil, nil, fmt.Errorf("env_vars_unset must be an array")
		}
		for _, item := range arr {
			s, ok := item.(string)
			if !ok {
				return nil, nil, fmt.Errorf("env_vars_unset items must be strings")
			}
			unset = append(unset, s)
		}
	}
	return set, unset, nil
}
