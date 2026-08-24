package handlers

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/sshproxy"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

// searchConfigSSHWait mirrors pushMemoryConfig's wait budget: long enough to
// ride out a container restart triggered by the same settings/instance save
// that called this, since openclaw.json lives on the persisted home PVC and
// survives it.
const searchConfigSSHWait = 120 * time.Second

// searchProviderPluginSpecs maps a managed web-search provider selection to
// the npm package that provides it. Like Slack/Discord, Brave ships as a
// separate package (see docs/plugins/plugin-inventory.md: "brave — npm;
// ClawHub") rather than inside the core openclaw npm package, so selecting it
// requires an install step on the agent, not just a config write.
//
// The provider id doubles as the OpenClaw plugin id today (both "brave").
// A future provider whose plugin id differs from its tools.web.search.provider
// value would need a second map; not needed yet.
var searchProviderPluginSpecs = map[string]string{
	"brave": "@openclaw/brave-plugin",
}

// isValidSearchProvider reports whether v is a search provider Claworc knows
// how to configure. "" means "inherit" (per-instance) or "leave OpenClaw's
// own auto-detection alone" (global default).
func isValidSearchProvider(v string) bool {
	return v == "" || v == "brave"
}

// effectiveSearchProvider resolves the web-search provider for an instance:
// per-instance override wins, otherwise the global default, otherwise ""
// (OpenClaw auto-detects from whatever credentials happen to be configured —
// Claworc does not touch tools.web.search.provider in that case).
func effectiveSearchProvider(inst *database.Instance) string {
	if inst.SearchProvider != "" {
		return inst.SearchProvider
	}
	if v, err := database.GetSetting("default_search_provider"); err == nil && isValidSearchProvider(v) {
		return v
	}
	return ""
}

// effectiveBraveAPIKeyPlain resolves the Brave API key for an instance:
// per-instance override wins (Instance.BraveAPIKey), otherwise the global
// key (Settings.brave_api_key). Returns "" if neither is configured.
func effectiveBraveAPIKeyPlain(inst *database.Instance) string {
	if inst.BraveAPIKey != "" {
		if plain, err := utils.Decrypt(inst.BraveAPIKey); err == nil && plain != "" {
			return plain
		}
	}
	raw, err := database.GetSetting("brave_api_key")
	if err != nil || raw == "" {
		return ""
	}
	plain, err := utils.Decrypt(raw)
	if err != nil {
		return ""
	}
	return plain
}

// buildSearchEnvVars returns the env vars buildCreateParams should inject for
// the instance's effective search-provider selection. The Brave API key rides
// the container environment (BRAVE_API_KEY) the same way Slack and Discord
// tokens do, and that env var is the *only* place it lives on the agent — the
// pushed OpenClaw config never references it (see buildSearchConfig).
// Returns an empty map when no managed provider is selected, so a
// BRAVE_API_KEY a user set through the generic env-vars editor for their own
// reasons is left alone.
func buildSearchEnvVars(inst *database.Instance) map[string]string {
	out := map[string]string{}
	if effectiveSearchProvider(inst) != "brave" {
		return out
	}
	if key := effectiveBraveAPIKeyPlain(inst); key != "" {
		out["BRAVE_API_KEY"] = key
	}
	return out
}

// buildSearchConfig resolves the OpenClaw config subtree Claworc pushes for
// the instance's effective search-provider selection. ok=false means no
// managed provider is selected — nothing to push, and Claworc's own paths
// have to be torn back down (see applySearchConfig).
//
// The API key is deliberately absent from this subtree. The Brave plugin
// resolves its key as `configured apiKey ?? env BRAVE_API_KEY` (see
// brave-web-search-provider.runtime, and the `envVars: ["BRAVE_API_KEY"]`
// declaration in web-search-shared), so the env var Claworc already injects
// via buildSearchEnvVars is sufficient on its own — exactly how Slack and
// Discord tokens reach the agent.
//
// Writing an apiKey SecretRef here instead was actively harmful. The config
// lives on the persisted home volume while the env var only reaches the
// container on (re)create, so any push that landed before the env var did
// left the agent with a ref it could not resolve — and an unresolved
// SecretRef is a *startup-fatal* error, not a degraded tool:
//
//	Gateway failed to start: required secrets are unavailable.
//	[WEB_SEARCH_KEY_UNRESOLVED_NO_FALLBACK]
//	plugins.entries.brave.config.webSearch.apiKey SecretRef is unresolved
//
// i.e. selecting a search provider could brick the whole agent, not just web
// search. Relying on the plugin's env fallback keeps a missing key to what it
// should be: web_search returns "needs a Brave Search API key" and every
// other subsystem boots normally.
func buildSearchConfig(inst *database.Instance) (provider string, webSearchCfg map[string]interface{}, ok bool) {
	provider = effectiveSearchProvider(inst)
	if provider != "brave" {
		return provider, nil, false
	}
	// `mode` selects Brave's response shape ("web" vs "llm-context") and is
	// read straight off this subtree by resolveBraveMode, so it is a real
	// setting rather than a placeholder.
	webSearchCfg = map[string]interface{}{
		"mode": "web",
	}
	return provider, webSearchCfg, true
}

// searchPluginPresent reports whether pluginID is discoverable in the
// decoded `plugins list --json` payload. known=false means the payload could
// not be parsed and the caller should not treat that as "missing".
func searchPluginPresent(stdout, pluginID string) (present bool, known bool) {
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

// ensureSearchPluginInstalled installs the plugin backing a managed search
// provider when the agent doesn't already have it. Runs inline (not
// fire-and-forget): applySearchConfig is already off the request path, and
// the config it is about to push depends on the plugin being discoverable.
//
// Only a confirmed-absent plugin is installed — same "never guess" rule as
// EnsureChannelPluginInstalled. Returns installed=true only when this call
// actually performed a fresh install, so the caller knows a gateway restart
// is needed to make the agent discover it.
func ensureSearchPluginInstalled(ctx context.Context, agent sshproxy.Instance, name, provider string) (installed bool) {
	spec, ok := searchProviderPluginSpecs[provider]
	if !ok {
		return false
	}
	stdout, _, code, err := agent.ExecOpenclaw(ctx, "plugins", "list", "--json")
	if err != nil || code != 0 {
		log.Printf("search-config: %s: could not list installed plugins, skipping %s install check: %v", name, provider, err)
		return false
	}
	present, known := searchPluginPresent(stdout, provider)
	if !known {
		log.Printf("search-config: %s: could not read the agent's plugin list, skipping %s install check", name, provider)
		return false
	}
	if present {
		return false
	}
	log.Printf("search-config: %s: installing %s for the %s search provider", name, spec, provider)
	_, stderr, code, err := agent.ExecOpenclaw(ctx, "plugins", "install", spec, "--acknowledge-clawhub-risk")
	if err != nil {
		log.Printf("search-config: %s: %s install failed: %v", name, spec, err)
		return false
	}
	if code != 0 {
		log.Printf("search-config: %s: %s install exited %d: %s", name, spec, code, utils.SanitizeForLog(stderr))
		return false
	}
	return true
}

// searchConfigSet writes one config path on the agent and logs any failure.
// what names the write for the log line. Returns false when the write did not
// land.
//
// `--json` is passed only for values that really are JSON. It is an alias for
// `--strict-json`, so handing it a bare string makes OpenClaw reject the whole
// write:
//
//	Error: Could not parse "brave" as JSON for --strict-json.
//	Unexpected token 'b', "brave" is not valid JSON.
//	... For plain strings, omit --strict-json.
//
// That is how tools.web.search.provider silently never got written.
func searchConfigSet(ctx context.Context, agent sshproxy.Instance, name, what, path, value string, extraArgs ...string) bool {
	args := append([]string{"config", "set", path, value}, extraArgs...)
	_, stderr, code, err := agent.ExecOpenclaw(ctx, args...)
	if err != nil {
		log.Printf("search-config: %s: set %s: %v", name, what, err)
		return false
	}
	if code != 0 {
		log.Printf("search-config: %s: set %s failed: %s", name, what, utils.SanitizeForLog(stderr))
		return false
	}
	return true
}

// searchConfigUnset clears one Claworc-owned config path on the agent.
//
// `config unset` takes no `--json` (OpenClaw answers "--json can only be used
// with --dry-run" and writes nothing), and it exits non-zero with "Config path
// not found" when the path was never set — which is the normal case for an
// agent that never had a managed provider. Both were previously treated as
// hard failures, so the entire switch-back-to-auto teardown was dead code.
//
// Returns whether the path is now clear: an absent path counts as success,
// since the caller only wants it gone.
func searchConfigUnset(ctx context.Context, agent sshproxy.Instance, name, path string) bool {
	_, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "unset", path)
	if err != nil {
		log.Printf("search-config: %s: unset %s: %v", name, path, err)
		return false
	}
	if code == 0 {
		return true
	}
	if strings.Contains(stderr, "Config path not found") {
		return true // never set on this agent; nothing to clear
	}
	log.Printf("search-config: %s: unset %s failed: %s", name, path, utils.SanitizeForLog(stderr))
	return false
}

// applySearchConfig reconciles the agent's search-provider config over SSH,
// in both directions: it writes Claworc's paths when a managed provider is
// selected and clears them when the selection goes back to "auto".
//
// tools.web.search.* and plugins.entries.<id>.config are hot-reloadable (see
// `openclaw config schema.lookup`: reloadKind "none"/"hot"), so an existing,
// already-installed plugin needs no gateway restart here — only a fresh
// install does, mirroring EnsureChannelPluginInstalled. Best-effort: failures
// are logged, same pattern as applyMemoryConfig.
//
// Nothing here carries the API key; it reaches the agent only as the
// BRAVE_API_KEY container env var (see buildSearchConfig for why).
func applySearchConfig(ctx context.Context, agent sshproxy.Instance, name string, inst *database.Instance) {
	name = utils.SanitizeForLog(name)
	provider, webSearchCfg, ok := buildSearchConfig(inst)

	if !ok {
		// Back to "auto": drop the paths Claworc owns so a provider it
		// previously pinned stops being used. Unsetting an absent path is a
		// tolerated no-op, so this is safe to issue unconditionally.
		searchConfigUnset(ctx, agent, name, "tools.web.search.provider")
		for pluginID := range searchProviderPluginSpecs {
			searchConfigUnset(ctx, agent, name, "plugins.entries."+pluginID+".enabled")
		}
		return
	}

	installedNow := ensureSearchPluginInstalled(ctx, agent, name, provider)

	webSearchJSON, err := json.Marshal(webSearchCfg)
	if err != nil {
		log.Printf("search-config: %s: marshal %s webSearch config: %v", name, provider, err)
		return
	}
	if !searchConfigSet(ctx, agent, name, provider+" webSearch config",
		"plugins.entries."+provider+".config.webSearch", string(webSearchJSON), "--replace", "--json") {
		return
	}
	searchConfigSet(ctx, agent, name, provider+" plugin enabled",
		"plugins.entries."+provider+".enabled", "true", "--json")
	// Plain string value — no --json (see searchConfigSet).
	searchConfigSet(ctx, agent, name, "tools.web.search.provider",
		"tools.web.search.provider", provider)

	if installedNow {
		if _, _, _, err := agent.ExecOpenclaw(ctx, "gateway", "stop"); err != nil {
			log.Printf("search-config: %s: installed %s but could not restart the gateway: %v", name, provider, err)
		}
	}
}

// pushSearchConfig is the async best-effort wrapper around applySearchConfig
// for a running instance, mirroring pushMemoryConfig/pushSlackConfig.
func pushSearchConfig(instanceID uint, name string) {
	if SSHMgr == nil {
		return
	}
	go func() {
		ctx := context.Background()
		sshClient, err := SSHMgr.WaitForSSH(ctx, instanceID, searchConfigSSHWait)
		if err != nil {
			log.Printf("search-config: no SSH connection for instance %d, skipping push: %v", instanceID, err)
			return
		}
		var inst database.Instance
		if err := database.DB.First(&inst, instanceID).Error; err != nil {
			return
		}
		applySearchConfig(ctx, sshproxy.NewSSHInstance(sshClient), name, &inst)
	}()
}

// pushSearchConfigForRunningInstances reconciles the search-provider config
// of every running, non-legacy instance. Used when the global default
// changes (default_search_provider or brave_api_key) so instances that
// inherit the global value pick it up without a per-instance edit.
func pushSearchConfigForRunningInstances() {
	var running []database.Instance
	database.DB.Where("status = ?", "running").Find(&running)
	for i := range running {
		if database.IsLegacyEmbedded(running[i].ContainerImage) {
			continue
		}
		pushSearchConfig(running[i].ID, running[i].Name)
	}
}
