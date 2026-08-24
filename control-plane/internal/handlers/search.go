package handlers

import (
	"context"
	"encoding/json"
	"log"
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
// the instance's effective search-provider selection. The Brave API key
// rides the container environment (BRAVE_API_KEY) the same way every other
// provider key here does; the agent's OpenClaw config only ever holds a
// SecretRef pointing at that env var (see buildSearchConfig), never the
// plaintext key. Returns an empty map when no managed provider is selected,
// so a BRAVE_API_KEY a user set through the generic env-vars editor for
// their own reasons is left alone.
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
// managed provider is selected — nothing to push, and nothing Claworc owns
// to unset either.
func buildSearchConfig(inst *database.Instance) (provider string, webSearchCfg map[string]interface{}, ok bool) {
	provider = effectiveSearchProvider(inst)
	if provider != "brave" {
		return provider, nil, false
	}
	webSearchCfg = map[string]interface{}{
		"mode": "web",
	}
	if effectiveBraveAPIKeyPlain(inst) != "" {
		webSearchCfg["apiKey"] = map[string]interface{}{
			"source":   "env",
			"provider": "default",
			"id":       "BRAVE_API_KEY",
		}
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

// applySearchConfig pushes the resolved search-provider config to the agent
// over SSH. tools.web.search.* and plugins.entries.<id>.config are both
// hot-reloadable (see `openclaw config schema.lookup`: reloadKind "none"/
// "hot"), so an existing, already-installed plugin needs no gateway restart
// here — only a fresh install does, mirroring EnsureChannelPluginInstalled.
// Best-effort: failures are logged, same pattern as applyMemoryConfig.
func applySearchConfig(ctx context.Context, agent sshproxy.Instance, name string, inst *database.Instance) {
	name = utils.SanitizeForLog(name)
	provider, webSearchCfg, ok := buildSearchConfig(inst)

	if !ok {
		// No managed provider selected. Unconditionally unsetting on every
		// push would be a wasted round-trip for the common case (an
		// instance that never touched search config), but config unset is
		// itself a no-op when the path isn't set, so it's safe to always
		// issue — this only matters for the switch-back-to-auto path.
		if _, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "unset", "tools.web.search.provider", "--json"); err != nil {
			log.Printf("search-config: unset provider for %s: %v", name, err)
		} else if code != 0 {
			log.Printf("search-config: unset provider for %s failed: %s", name, utils.SanitizeForLog(stderr))
		}
		return
	}

	installedNow := ensureSearchPluginInstalled(ctx, agent, name, provider)

	webSearchJSON, err := json.Marshal(webSearchCfg)
	if err != nil {
		log.Printf("search-config: marshal %s webSearch config for %s: %v", provider, name, err)
		return
	}
	if _, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "set", "plugins.entries."+provider+".config.webSearch", string(webSearchJSON), "--replace", "--json"); err != nil {
		log.Printf("search-config: set %s webSearch config for %s: %v", provider, name, err)
		return
	} else if code != 0 {
		log.Printf("search-config: set %s webSearch config for %s failed: %s", provider, name, utils.SanitizeForLog(stderr))
		return
	}
	if _, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "set", "plugins.entries."+provider+".enabled", "true", "--json"); err != nil {
		log.Printf("search-config: enable %s plugin for %s: %v", provider, name, err)
	} else if code != 0 {
		log.Printf("search-config: enable %s plugin for %s failed: %s", provider, name, utils.SanitizeForLog(stderr))
	}
	if _, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "set", "tools.web.search.provider", provider, "--json"); err != nil {
		log.Printf("search-config: set tools.web.search.provider for %s: %v", name, err)
	} else if code != 0 {
		log.Printf("search-config: set tools.web.search.provider for %s failed: %s", name, utils.SanitizeForLog(stderr))
	}

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
