package handlers

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/sshproxy"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

// Nothing has to be installed for a channel to work. Both extensions ship
// inside the openclaw npm package the agent image installs globally, and
// OpenClaw enables them for itself: at every gateway start
// applyPluginAutoEnable turns on any plugin whose channel looks configured,
// which for Slack and Discord means either the token env var or a non-empty
// channels.<id> block -- Claworc writes both.
//
// What was missing is the readback. Enabling a channel was a write we never
// confirmed, so a plugin that failed to load left the settings card showing
// green while the bot silently never answered -- the same shape as the env
// var bug in docs/env-propagation.md. This asks the agent what actually
// happened rather than assuming.

// channelPluginState is the coarse outcome shown on a channel settings card.
type channelPluginState string

const (
	// pluginLoaded: OpenClaw loaded the plugin, so the channel can run.
	pluginLoaded channelPluginState = "loaded"
	// pluginDisabled: discovered but not enabled. Auto-enable deliberately
	// skips a plugin when plugins.enabled is false, when the id is in
	// plugins.deny, or when it is explicitly disabled in plugins.entries --
	// opt-outs a hand-edited agent config is entitled to make, and which
	// Claworc reports rather than overrides.
	pluginDisabled channelPluginState = "disabled"
	// pluginError: discovered but failed to load; Detail carries the reason.
	pluginError channelPluginState = "error"
	// pluginMissing: not discovered at all, meaning the bundled extensions
	// directory is not where OpenClaw looks for it (a hand-built image, or
	// OPENCLAW_BUNDLED_PLUGINS_DIR pointing somewhere else).
	pluginMissing channelPluginState = "missing"
	// pluginUnknown: the agent could not be asked. Not a problem report --
	// the plugin may well be fine.
	pluginUnknown channelPluginState = "unknown"
)

type channelPluginStatus struct {
	State  channelPluginState `json:"state"`
	Detail string             `json:"detail,omitempty"`
}

// pluginStatusTimeout bounds the readback. `plugins list` loads every plugin
// in a fresh CLI process, so it is not instant, but it runs inline in a GET
// and must not hold the request open indefinitely.
const pluginStatusTimeout = 8 * time.Second

// openclawPluginsList is the subset of `openclaw plugins list --json` we read.
type openclawPluginsList struct {
	Plugins []struct {
		ID         string   `json:"id"`
		Status     string   `json:"status"`
		Error      string   `json:"error"`
		ChannelIDs []string `json:"channelIds"`
	} `json:"plugins"`
}

// channelPluginStatusFor asks the agent whether the plugin backing channelID
// ("slack" / "discord") actually loaded.
//
// Best-effort by construction: every failure to ask returns pluginUnknown
// rather than a scary state, because "we could not check" and "it is broken"
// are different claims and the card must not conflate them.
func channelPluginStatusFor(instanceID uint, channelID string) channelPluginStatus {
	if SSHMgr == nil {
		return channelPluginStatus{State: pluginUnknown, Detail: "SSH is not configured on the control plane"}
	}
	// GetConnection, deliberately, not WaitForSSH: this runs inline in a GET
	// and must never block waiting for an agent to come up. A stopped or
	// still-booting agent simply cannot be asked yet.
	client, ok := SSHMgr.GetConnection(instanceID)
	if !ok {
		return channelPluginStatus{State: pluginUnknown, Detail: "Agent is not reachable over SSH"}
	}

	type execResult struct {
		stdout string
		code   int
		err    error
	}
	// ExecOpenclaw takes a context but the RunCommand beneath it does not
	// honor one, so the deadline has to be enforced here. A session on a dead
	// client errors out on its own eventually, so the goroutine we stop
	// waiting for is bounded even though we abandon it.
	done := make(chan execResult, 1)
	go func() {
		stdout, _, code, err := sshproxy.NewSSHInstance(client).
			ExecOpenclaw(context.Background(), "plugins", "list", "--json")
		done <- execResult{stdout: stdout, code: code, err: err}
	}()

	var res execResult
	select {
	case res = <-done:
	case <-time.After(pluginStatusTimeout):
		return channelPluginStatus{State: pluginUnknown, Detail: "Timed out asking the agent"}
	}

	if res.err != nil {
		log.Printf("plugin-status: instance %d: %v", instanceID, res.err)
		return channelPluginStatus{State: pluginUnknown, Detail: "Could not query the agent"}
	}
	if res.code != 0 {
		// An older agent image may not have `plugins list --json` at all.
		return channelPluginStatus{State: pluginUnknown, Detail: "Agent does not support plugin status"}
	}

	status, err := parseChannelPluginStatus(res.stdout, channelID)
	if err != nil {
		log.Printf("plugin-status: instance %d: unparseable plugins list: %v", instanceID, err)
		return channelPluginStatus{State: pluginUnknown, Detail: "Could not read the agent's plugin list"}
	}
	return status
}

// parseChannelPluginStatus maps `openclaw plugins list --json` output to the
// state for one channel. Split out from the SSH plumbing so the mapping --
// the part with actual decisions in it -- is testable without an agent.
//
// A plugin is matched by its own id or by the channel it registers: the
// bundled extensions happen to use the channel name as their id, but the
// manifest's channels list is the real contract, and a repackaged plugin
// under a different id should still be recognised.
func parseChannelPluginStatus(stdout, channelID string) (channelPluginStatus, error) {
	var listed openclawPluginsList
	if err := json.Unmarshal([]byte(extractJSONObject(stdout)), &listed); err != nil {
		return channelPluginStatus{}, err
	}
	for _, p := range listed.Plugins {
		if p.ID != channelID && !containsString(p.ChannelIDs, channelID) {
			continue
		}
		switch p.Status {
		case "loaded":
			return channelPluginStatus{State: pluginLoaded}, nil
		case "error":
			return channelPluginStatus{State: pluginError, Detail: utils.SanitizeForLog(p.Error)}, nil
		default:
			// "disabled", or any status a future OpenClaw adds: the plugin is
			// there but not running, which is the actionable distinction.
			//
			// OpenClaw puts the reason in the same `error` field it uses for
			// load failures ("not in allowlist", "disabled in config",
			// "blocked by denylist", "bundled (disabled by default)"), and
			// that string is the whole diagnostic -- without it the card can
			// only say "off", leaving the operator to guess which of four
			// switches did it.
			return channelPluginStatus{State: pluginDisabled, Detail: utils.SanitizeForLog(p.Error)}, nil
		}
	}
	return channelPluginStatus{State: pluginMissing}, nil
}

// extractJSONObject trims anything the CLI printed around its JSON payload.
// `plugins list --json` writes only the object today, but the agent's shell
// profile can emit a banner ahead of it, which would otherwise fail the
// unmarshal and report a healthy plugin as unknown.
func extractJSONObject(out string) string {
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end < start {
		return out
	}
	return out[start : end+1]
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
