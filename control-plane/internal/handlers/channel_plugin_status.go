package handlers

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/sshproxy"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
	gossh "golang.org/x/crypto/ssh"
)

// Slack and Discord are no longer part of OpenClaw. They used to ship inside
// the npm package, but were split into separate @openclaw/<channel> packages
// (Discord around host 2026.4.10, Slack around 2026.5.12) and are now excluded
// from the published tarball by name. The agent image only runs
// `npm install -g openclaw`, and the package's postinstall deliberately
// installs no plugin packages, so a fresh agent has neither.
//
// That makes two things Claworc has to handle rather than assume:
//
//   - Installation. ensureChannelPluginInstalled puts the plugin on the agent
//     when a channel is enabled. Auto-enable cannot help here: OpenClaw's
//     applyPluginAutoEnable only flips plugins.entries.<id>.enabled for
//     plugins it *discovered*, and an uninstalled plugin is not discovered.
//   - Confirmation. Enabling a channel used to be a write we never checked,
//     so a plugin that never loaded left the settings card showing green
//     while the bot silently never answered -- the same shape as the env var
//     bug in docs/env-propagation.md. channelPluginStatusFor asks the agent
//     what actually happened.
//
// See docs/channel-plugins.md.

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
	// pluginChecking: a probe is running and there is no cached answer yet.
	// Distinct from pluginUnknown so the UI can say "ask again shortly"
	// instead of "could not reach the agent", and poll rather than give up.
	pluginChecking channelPluginState = "checking"
)

type channelPluginStatus struct {
	State  channelPluginState `json:"state"`
	Detail string             `json:"detail,omitempty"`
}

// pluginStatusProbeTimeout bounds one probe.
//
// This was 8s, chosen without measuring, and it timed out every time. The work
// behind `plugins list --json` is not small: ExecOpenclaw runs it through
// `su - claworc`, a *login* shell that sources a profile reaching into the
// Homebrew PVC, and the command then boots Node and has OpenClaw discover and
// load every plugin it can find. Tens of seconds on a busy agent is ordinary
// rather than pathological, so the budget is now generous -- and, crucially,
// no longer sits on a request (see channelPluginStatusCached).
const pluginStatusProbeTimeout = 90 * time.Second

// pluginStatusCacheTTL is how long a probe result is served before another
// probe is run. Plugin state only changes when something installs, enables or
// breaks a plugin, so it does not need to be fresh to the second.
const pluginStatusCacheTTL = 2 * time.Minute

// openclawPluginsList is the subset of `openclaw plugins list --json` we read.
type openclawPluginsList struct {
	Plugins []struct {
		ID         string   `json:"id"`
		Status     string   `json:"status"`
		Error      string   `json:"error"`
		ChannelIDs []string `json:"channelIds"`
	} `json:"plugins"`
}

// probeChannelPluginStatus asks the agent whether the plugin backing channelID
// ("slack" / "discord") actually loaded. Synchronous and slow -- callers on a
// request path want channelPluginStatusCached instead.
//
// Best-effort by construction: every failure to ask returns pluginUnknown
// rather than a scary state, because "we could not check" and "it is broken"
// are different claims and the card must not conflate them.
func probeChannelPluginStatus(instanceID uint, channelID string) channelPluginStatus {
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

	res, timedOut := execOpenclawBounded(client, pluginStatusProbeTimeout, "plugins", "list", "--json")
	if timedOut {
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

// openclawExecResult is the outcome of one bounded openclaw invocation.
type openclawExecResult struct {
	stdout string
	stderr string
	code   int
	err    error
}

// execOpenclawBounded runs an openclaw command with a wall-clock bound.
//
// ExecOpenclaw accepts a context but the RunCommand beneath it does not honor
// one, so the deadline has to be enforced out here. The abandoned goroutine is
// still bounded in practice: a session on a dead client errors out on its own
// once TCP gives up. Returns timedOut=true when the bound was hit, which
// callers must treat as "no answer", never as a negative answer.
func execOpenclawBounded(client *gossh.Client, timeout time.Duration, args ...string) (res openclawExecResult, timedOut bool) {
	done := make(chan openclawExecResult, 1)
	go func() {
		stdout, stderr, code, err := sshproxy.NewSSHInstance(client).
			ExecOpenclaw(context.Background(), args...)
		done <- openclawExecResult{stdout: stdout, stderr: stderr, code: code, err: err}
	}()
	select {
	case res = <-done:
		return res, false
	case <-time.After(timeout):
		return openclawExecResult{}, true
	}
}
