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
	"sync"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/database"
	"github.com/gluk-w/claworc/control-plane/internal/middleware"
	"github.com/gluk-w/claworc/control-plane/internal/sshproxy"
	"github.com/gluk-w/claworc/control-plane/internal/taskmanager"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
	"github.com/go-chi/chi/v5"
)

// This file generalizes the install/status machinery channel_plugin_*.go
// built for Discord and Slack into a plugin manager for *any* OpenClaw
// plugin -- npm package, ClawHub skill/plugin, or git repo -- so an agent
// can pick up a plugin like lossless-claw (@martian-engineering/lossless-claw)
// that is not, and will likely never be, in OpenClaw's own channel catalog.
//
// It deliberately shares the same constraints documented in
// docs/channel-plugins.md:
//   - Never probe or install inline on a request. `openclaw plugins list
//     --json` and `openclaw plugins install <spec>` are both slow (login
//     shell + Node boot, or an npm install), so both run off the request
//     path with a cached/async result.
//   - Never touch plugins.allow / plugins.deny. Those are the operator's
//     security controls; Claworc reports what they produce but never
//     widens or narrows them on the operator's behalf.
//   - "unknown" (could not ask the agent) is never treated as "absent" --
//     acting on it would mean installing/uninstalling on a guess whenever
//     an agent is briefly unreachable.

// pluginIDRegex matches an OpenClaw plugin id as reported by `plugins list`
// (lowercase letters, digits, hyphens). Used to validate path params before
// they are shell-quoted into an SSH command.
var pluginIDRegex = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// pluginListProbeTimeout / pluginListCacheTTL mirror the channel-plugin probe
// budgets (see channel_plugin_status.go) -- same command, same cost.
const (
	pluginListProbeTimeout = 90 * time.Second
	pluginListCacheTTL     = 2 * time.Minute
	pluginActionSSHTimeout = 30 * time.Second
	pluginInstallTaskSSHWait = 3 * time.Minute
)

// pluginSummary is the subset of one `openclaw plugins list --json` entry
// Claworc's UI needs. The full entry carries dozens of provider-id arrays
// that are irrelevant to "is this plugin on and healthy".
type pluginSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Version      string `json:"version,omitempty"`
	Origin       string `json:"origin,omitempty"` // "bundled" | "global" | "local" | ...
	Status       string `json:"status"`           // "loaded" | "disabled" | "error"
	Enabled      bool   `json:"enabled"`
	Error        string `json:"error,omitempty"`
	ConfigSchema bool   `json:"config_schema"`
	// ChannelIDs is non-empty for channel plugins (discord/slack/...); the UI
	// points the operator at the dedicated Slack/Discord cards for those
	// instead of the generic config editor.
	ChannelIDs []string `json:"channel_ids,omitempty"`
}

type instancePluginsList struct {
	Plugins []pluginSummary `json:"plugins"`
}

// rawOpenclawPluginEntry is the on-wire shape of one `plugins list --json`
// entry, trimmed to the fields pluginSummary reads.
type rawOpenclawPluginEntry struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Origin       string   `json:"origin"`
	Status       string   `json:"status"`
	Enabled      bool     `json:"enabled"`
	Error        string   `json:"error"`
	ConfigSchema bool     `json:"configSchema"`
	ChannelIDs   []string `json:"channelIds"`
}

type rawOpenclawPluginsList struct {
	Plugins []rawOpenclawPluginEntry `json:"plugins"`
}

func parsePluginsList(stdout string) ([]pluginSummary, error) {
	var raw rawOpenclawPluginsList
	if err := json.Unmarshal([]byte(extractJSONObject(stdout)), &raw); err != nil {
		return nil, err
	}
	out := make([]pluginSummary, 0, len(raw.Plugins))
	for _, p := range raw.Plugins {
		out = append(out, pluginSummary{
			ID:           p.ID,
			Name:         p.Name,
			Version:      p.Version,
			Origin:       p.Origin,
			Status:       p.Status,
			Enabled:      p.Enabled,
			Error:        utils.SanitizeForLog(p.Error),
			ConfigSchema: p.ConfigSchema,
			ChannelIDs:   p.ChannelIDs,
		})
	}
	return out, nil
}

// probeInstancePlugins asks the agent for its full plugin list. Synchronous
// and slow -- same cost profile as probeChannelPluginStatus, and for the same
// reason (ExecOpenclaw runs through a login shell that boots Node and has
// OpenClaw discover/load every plugin it can find).
func probeInstancePlugins(instanceID uint) (instancePluginsList, string) {
	if SSHMgr == nil {
		return instancePluginsList{}, "SSH is not configured on the control plane"
	}
	client, ok := SSHMgr.GetConnection(instanceID)
	if !ok {
		return instancePluginsList{}, "Agent is not reachable over SSH"
	}
	res, timedOut := execOpenclawBounded(client, pluginListProbeTimeout, "plugins", "list", "--json")
	if timedOut {
		return instancePluginsList{}, "Timed out asking the agent"
	}
	if res.err != nil {
		log.Printf("plugins-list: instance %d: %v", instanceID, res.err)
		return instancePluginsList{}, "Could not query the agent"
	}
	if res.code != 0 {
		return instancePluginsList{}, "Agent does not support plugin listing"
	}
	plugins, err := parsePluginsList(res.stdout)
	if err != nil {
		log.Printf("plugins-list: instance %d: unparseable plugins list: %v", instanceID, err)
		return instancePluginsList{}, "Could not read the agent's plugin list"
	}
	return instancePluginsList{Plugins: plugins}, ""
}

// --- cache (mirrors channel_plugin_cache.go, keyed by instance only) -------

type cachedPluginsList struct {
	list  instancePluginsList
	error string
	at    time.Time
}

var (
	pluginsListMu      sync.Mutex
	pluginsListCache   = map[uint]cachedPluginsList{}
	pluginsListRefresh = map[uint]bool{}
)

// instancePluginsCached returns the last known plugin list immediately,
// refreshing in the background when it is stale or absent. See
// channelPluginStatusCached for the same "stale beats blank" reasoning.
func instancePluginsCached(instanceID uint) (instancePluginsList, string, bool) {
	pluginsListMu.Lock()
	entry, hasEntry := pluginsListCache[instanceID]
	fresh := hasEntry && time.Since(entry.at) < pluginListCacheTTL
	startRefresh := !fresh && !pluginsListRefresh[instanceID]
	if startRefresh {
		pluginsListRefresh[instanceID] = true
	}
	pluginsListMu.Unlock()

	if startRefresh {
		go func() {
			list, errMsg := probeInstancePlugins(instanceID)
			pluginsListMu.Lock()
			// A failed probe must not blank out a previously good answer --
			// keep serving the stale list until a real answer arrives.
			if errMsg == "" || !hasEntry {
				pluginsListCache[instanceID] = cachedPluginsList{list: list, error: errMsg, at: time.Now()}
			}
			delete(pluginsListRefresh, instanceID)
			pluginsListMu.Unlock()
		}()
	}

	if hasEntry {
		return entry.list, entry.error, true
	}
	return instancePluginsList{}, "", false
}

func invalidateInstancePlugins(instanceID uint) {
	pluginsListMu.Lock()
	delete(pluginsListCache, instanceID)
	pluginsListMu.Unlock()
}

// --- HTTP handlers ----------------------------------------------------------

// GET /api/v1/instances/{id}/plugins
//
// Same shape as the Discord/Slack plugin readback: cached, refreshed in the
// background, "checking" only until the first probe lands.
func ListInstancePlugins(w http.ResponseWriter, r *http.Request) {
	instanceID, ok := parseInstanceIDParam(w, r)
	if !ok {
		return
	}
	if !middleware.CanAccessInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, "Access denied")
		return
	}
	list, errMsg, known := instancePluginsCached(instanceID)
	resp := map[string]interface{}{
		"plugins": list.Plugins,
	}
	if resp["plugins"] == nil {
		resp["plugins"] = []pluginSummary{}
	}
	switch {
	case !known:
		resp["state"] = "checking"
	case errMsg != "":
		resp["state"] = "unknown"
		resp["detail"] = errMsg
	default:
		resp["state"] = "ok"
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseInstanceIDParam(w http.ResponseWriter, r *http.Request) (uint, bool) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "Invalid instance ID")
		return 0, false
	}
	return uint(id), true
}

// installPluginRequest is the body for POST /instances/{id}/plugins. Spec is
// passed straight to `openclaw plugins install <spec>` -- OpenClaw itself
// accepts an npm spec, a `clawhub:<package>` reference, a git URL, or a local
// path, so Claworc does not need to understand or validate the spec's shape
// beyond stripping obviously dangerous shell content (ExecOpenclaw already
// shell-quotes the whole argument, so this is a sanity check, not the only
// line of defense).
type installPluginRequest struct {
	Spec string `json:"spec"`
}

// validatePluginSpec rejects the empty string and literal whitespace/newlines,
// which would either no-op or smuggle a second command through a shell that
// only ShellQuote, not this handler, is responsible for neutralizing anyway.
func validatePluginSpec(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return fmt.Errorf("plugin spec is required")
	}
	if strings.ContainsAny(spec, "\n\r") {
		return fmt.Errorf("plugin spec must not contain newlines")
	}
	return nil
}

// POST /api/v1/instances/{id}/plugins
//
// Installs a plugin by spec (npm package, `clawhub:<package>`, or git URL)
// and restarts the gateway so it is discovered. Async via TaskManager --
// this is an npm install inside the pod and can take minutes, exactly like
// EnsureChannelPluginInstalled, so it must never sit on the request path.
func InstallInstancePlugin(w http.ResponseWriter, r *http.Request) {
	instanceID, ok := parseInstanceIDParam(w, r)
	if !ok {
		return
	}
	if !middleware.CanMutateInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, "Access denied")
		return
	}
	var body installPluginRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	spec := strings.TrimSpace(body.Spec)
	if err := validatePluginSpec(spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if SSHMgr == nil {
		writeError(w, http.StatusServiceUnavailable, "SSH is not configured on the control plane")
		return
	}

	var inst database.Instance
	displayName := fmt.Sprintf("instance %d", instanceID)
	if err := database.DB.Select("display_name").First(&inst, instanceID).Error; err == nil {
		displayName = inst.DisplayName
	}

	run := func(ctx context.Context, h *taskmanager.Handle) error {
		if h != nil {
			h.UpdateMessage("connecting to agent")
		}
		client, err := SSHMgr.WaitForSSH(ctx, instanceID, pluginInstallTaskSSHWait)
		if err != nil {
			return fmt.Errorf("no SSH connection: %w", err)
		}
		if h != nil {
			h.UpdateMessage("installing " + spec)
		}
		res, timedOut := execOpenclawBounded(client, pluginInstallTimeout, "plugins", "install", spec, "--acknowledge-clawhub-risk")
		if timedOut {
			return fmt.Errorf("install timed out after %s", pluginInstallTimeout)
		}
		if res.err != nil {
			return fmt.Errorf("install failed: %w", res.err)
		}
		if res.code != 0 {
			return fmt.Errorf("install exited %d: %s", res.code, utils.SanitizeForLog(res.stderr))
		}
		if h != nil {
			h.UpdateMessage("restarting gateway")
		}
		if _, _, _, err := sshproxy.NewSSHInstance(client).ExecOpenclaw(ctx, "gateway", "stop"); err != nil {
			return fmt.Errorf("installed but could not restart the gateway: %w", err)
		}
		invalidateInstancePlugins(instanceID)
		return nil
	}

	if TaskMgr == nil {
		go func() { _ = run(context.Background(), nil) }()
		writeJSON(w, http.StatusAccepted, map[string]interface{}{"task_id": ""})
		return
	}
	taskID := TaskMgr.Start(taskmanager.StartOpts{
		Type:         taskmanager.TaskPluginInstall,
		InstanceID:   instanceID,
		UserID:       callerID(r),
		ResourceID:   spec,
		ResourceName: fmt.Sprintf("%s — %s", displayName, spec),
		Title:        fmt.Sprintf("Installing %s on %s", spec, displayName),
		Run: func(ctx context.Context, h *taskmanager.Handle) error {
			return run(ctx, h)
		},
	})
	writeJSON(w, http.StatusAccepted, map[string]interface{}{"task_id": taskID})
}

// pluginActionResult is the sync response for enable/disable (fast: a config
// write + gateway restart kick, not an npm install).
type pluginActionResult struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Restart bool   `json:"restarting,omitempty"`
}

func resolvePluginID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "pluginId")
	if !pluginIDRegex.MatchString(id) {
		writeError(w, http.StatusBadRequest, "Invalid plugin id")
		return "", false
	}
	return id, true
}

// runPluginConfigAction runs a fast (non-install) openclaw subcommand against
// the instance over SSH, then restarts the gateway so the change takes
// effect, mirroring applyDiscordConfig/applySlackConfig's config-set+restart
// pattern.
func runPluginConfigAction(instanceID uint, args ...string) pluginActionResult {
	if SSHMgr == nil {
		return pluginActionResult{Error: "SSH is not configured on the control plane"}
	}
	client, ok := SSHMgr.GetConnection(instanceID)
	if !ok {
		return pluginActionResult{Error: "Agent is not reachable over SSH"}
	}
	res, timedOut := execOpenclawBounded(client, pluginActionSSHTimeout, args...)
	if timedOut {
		return pluginActionResult{Error: "Timed out talking to the agent"}
	}
	if res.err != nil {
		return pluginActionResult{Error: res.err.Error()}
	}
	if res.code != 0 {
		return pluginActionResult{Error: utils.SanitizeForLog(res.stderr)}
	}
	if _, _, _, err := sshproxy.NewSSHInstance(client).ExecOpenclaw(context.Background(), "gateway", "stop"); err != nil {
		return pluginActionResult{OK: true, Error: "applied, but could not restart the gateway: " + err.Error()}
	}
	invalidateInstancePlugins(instanceID)
	return pluginActionResult{OK: true, Restart: true}
}

// POST /api/v1/instances/{id}/plugins/{pluginId}/enable
func EnableInstancePlugin(w http.ResponseWriter, r *http.Request) {
	instanceID, ok := parseInstanceIDParam(w, r)
	if !ok {
		return
	}
	if !middleware.CanMutateInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, "Access denied")
		return
	}
	pluginID, ok := resolvePluginID(w, r)
	if !ok {
		return
	}
	res := runPluginConfigAction(instanceID, "plugins", "enable", pluginID)
	writeJSON(w, http.StatusOK, res)
}

// POST /api/v1/instances/{id}/plugins/{pluginId}/disable
func DisableInstancePlugin(w http.ResponseWriter, r *http.Request) {
	instanceID, ok := parseInstanceIDParam(w, r)
	if !ok {
		return
	}
	if !middleware.CanMutateInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, "Access denied")
		return
	}
	pluginID, ok := resolvePluginID(w, r)
	if !ok {
		return
	}
	res := runPluginConfigAction(instanceID, "plugins", "disable", pluginID)
	writeJSON(w, http.StatusOK, res)
}

// DELETE /api/v1/instances/{id}/plugins/{pluginId}
//
// Uninstall is async via TaskManager for the same reason install is: it is a
// filesystem/npm operation inside the pod, not a config write.
func UninstallInstancePlugin(w http.ResponseWriter, r *http.Request) {
	instanceID, ok := parseInstanceIDParam(w, r)
	if !ok {
		return
	}
	if !middleware.CanMutateInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, "Access denied")
		return
	}
	pluginID, ok := resolvePluginID(w, r)
	if !ok {
		return
	}
	if SSHMgr == nil {
		writeError(w, http.StatusServiceUnavailable, "SSH is not configured on the control plane")
		return
	}

	var inst database.Instance
	displayName := fmt.Sprintf("instance %d", instanceID)
	if err := database.DB.Select("display_name").First(&inst, instanceID).Error; err == nil {
		displayName = inst.DisplayName
	}

	run := func(ctx context.Context, h *taskmanager.Handle) error {
		if h != nil {
			h.UpdateMessage("connecting to agent")
		}
		client, err := SSHMgr.WaitForSSH(ctx, instanceID, pluginInstallTaskSSHWait)
		if err != nil {
			return fmt.Errorf("no SSH connection: %w", err)
		}
		if h != nil {
			h.UpdateMessage("uninstalling " + pluginID)
		}
		res, timedOut := execOpenclawBounded(client, pluginInstallTimeout, "plugins", "uninstall", pluginID, "--force")
		if timedOut {
			return fmt.Errorf("uninstall timed out after %s", pluginInstallTimeout)
		}
		if res.err != nil {
			return fmt.Errorf("uninstall failed: %w", res.err)
		}
		if res.code != 0 {
			return fmt.Errorf("uninstall exited %d: %s", res.code, utils.SanitizeForLog(res.stderr))
		}
		if h != nil {
			h.UpdateMessage("restarting gateway")
		}
		if _, _, _, err := sshproxy.NewSSHInstance(client).ExecOpenclaw(ctx, "gateway", "stop"); err != nil {
			return fmt.Errorf("uninstalled but could not restart the gateway: %w", err)
		}
		invalidateInstancePlugins(instanceID)
		return nil
	}

	if TaskMgr == nil {
		go func() { _ = run(context.Background(), nil) }()
		writeJSON(w, http.StatusAccepted, map[string]interface{}{"task_id": ""})
		return
	}
	taskID := TaskMgr.Start(taskmanager.StartOpts{
		Type:         taskmanager.TaskPluginUninstall,
		InstanceID:   instanceID,
		UserID:       callerID(r),
		ResourceID:   pluginID,
		ResourceName: fmt.Sprintf("%s — %s", displayName, pluginID),
		Title:        fmt.Sprintf("Uninstalling %s from %s", pluginID, displayName),
		Run: func(ctx context.Context, h *taskmanager.Handle) error {
			return run(ctx, h)
		},
	})
	writeJSON(w, http.StatusAccepted, map[string]interface{}{"task_id": taskID})
}

// GET /api/v1/instances/{id}/plugins/{pluginId}/config
//
// Reads back `plugins.entries.<id>.config` as raw JSON. There is no generic
// per-plugin schema Claworc can render a form from, so the editor is a raw
// JSON blob -- the same tradeoff the in-browser skill file editor makes for
// SKILL.md.
func GetInstancePluginConfig(w http.ResponseWriter, r *http.Request) {
	instanceID, ok := parseInstanceIDParam(w, r)
	if !ok {
		return
	}
	if !middleware.CanAccessInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, "Access denied")
		return
	}
	pluginID, ok := resolvePluginID(w, r)
	if !ok {
		return
	}
	if SSHMgr == nil {
		writeError(w, http.StatusServiceUnavailable, "SSH is not configured on the control plane")
		return
	}
	client, ok := SSHMgr.GetConnection(instanceID)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "Agent is not reachable over SSH")
		return
	}
	path := "plugins.entries." + pluginID + ".config"
	res, timedOut := execOpenclawBounded(client, pluginActionSSHTimeout, "config", "get", path, "--json")
	if timedOut {
		writeError(w, http.StatusGatewayTimeout, "Timed out talking to the agent")
		return
	}
	if res.err != nil || res.code != 0 {
		// A plugin with no config entry yet is not an error -- render "{}"
		// so the editor has something sane to start from.
		writeJSON(w, http.StatusOK, map[string]interface{}{"config": "{}"})
		return
	}
	config := strings.TrimSpace(res.stdout)
	if config == "" {
		config = "{}"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"config": config})
}

type putPluginConfigRequest struct {
	Config string `json:"config"`
}

// PUT /api/v1/instances/{id}/plugins/{pluginId}/config
//
// Validates the body is JSON (openclaw's own --json parser would reject bad
// JSON anyway, but failing here gives a clearer error than an SSH round trip)
// then writes it and restarts the gateway.
func PutInstancePluginConfig(w http.ResponseWriter, r *http.Request) {
	instanceID, ok := parseInstanceIDParam(w, r)
	if !ok {
		return
	}
	if !middleware.CanMutateInstance(r, instanceID) {
		writeError(w, http.StatusForbidden, "Access denied")
		return
	}
	pluginID, ok := resolvePluginID(w, r)
	if !ok {
		return
	}
	var body putPluginConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(body.Config), &parsed); err != nil {
		writeError(w, http.StatusBadRequest, "Config must be valid JSON: "+err.Error())
		return
	}
	path := "plugins.entries." + pluginID + ".config"
	res := runPluginConfigAction(instanceID, "config", "set", path, body.Config, "--json")
	writeJSON(w, http.StatusOK, res)
}
