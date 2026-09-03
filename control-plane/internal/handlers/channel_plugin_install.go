package handlers

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gluk-w/claworc/control-plane/internal/sshproxy"
	"github.com/gluk-w/claworc/control-plane/internal/utils"
)

// channelPluginSpecs maps a channel to the npm package that provides it.
//
// These are the "official" entries in OpenClaw's own external channel catalog
// (scripts/lib/official-external-channel-catalog.json), which is what
// `openclaw plugins install` resolves against. Deliberately unpinned: the
// catalog does not pin them either, and each declares a minHostVersion, so the
// registry's latest is the version meant to pair with a current host. Pinning
// here would rot against the agent image's openclaw version instead.
var channelPluginSpecs = map[string]string{
	"discord": "@openclaw/discord",
	"slack":   "@openclaw/slack",
}

// pluginInstallTimeout bounds the install itself. This is an npm install
// inside the agent, over whatever egress the pod has, so it is generous --
// but it must still be bounded, or a pod with no route to the registry would
// hold the goroutine forever.
const pluginInstallTimeout = 5 * time.Minute

// pluginInstallSSHWait is how long to wait for a usable SSH connection.
// Longer than the status readback's zero patience on purpose: the caller may
// have just triggered a container restart for a token change, so the agent is
// expected to be away for a while.
const pluginInstallSSHWait = 3 * time.Minute

// inFlightPluginInstalls stops a second save from starting a duplicate npm
// install while the first is still running. Keyed by instance+channel; a
// duplicate is dropped rather than queued, because the survivor does the same
// work and the loser has nothing to add.
var inFlightPluginInstalls sync.Map

// EnsureChannelPluginInstalled installs the plugin backing channelID on an
// agent that does not have it, then restarts the gateway so OpenClaw's
// auto-enable pass picks it up.
//
// Asynchronous and best-effort. It runs an npm install inside the agent, which
// can take minutes and needs registry egress from the pod, so it must never be
// on the request path -- the caller returns immediately and the settings
// card's plugin readback reports the outcome on its next load.
//
// Only a confirmed-absent plugin is installed. An agent we could not ask
// (pluginUnknown) is left alone: "we could not check" is not evidence of
// absence, and installing on a guess would mean an npm install every time an
// agent happened to be unreachable.
func EnsureChannelPluginInstalled(instanceID uint, instanceName, channelID string) {
	spec, ok := channelPluginSpecs[channelID]
	if !ok || SSHMgr == nil {
		return
	}
	key := fmt.Sprintf("%d/%s", instanceID, channelID)
	if _, loaded := inFlightPluginInstalls.LoadOrStore(key, true); loaded {
		return
	}

	go func() {
		defer inFlightPluginInstalls.Delete(key)
		ctx := context.Background()
		name := utils.SanitizeForLog(instanceName)

		client, err := SSHMgr.WaitForSSH(ctx, instanceID, pluginInstallSSHWait)
		if err != nil {
			log.Printf("plugin-install: %s: no SSH connection, skipping %s install: %v", name, channelID, err)
			return
		}

		// Re-check against the agent rather than trusting the caller: the
		// plugin may already be there from an earlier enable or a manual
		// install, and reinstalling would restart the gateway for nothing.
		//
		// Probes synchronously on the full budget rather than reading the
		// cache. This goroutine can afford to wait, and it needs a real
		// answer: the cache may still be serving "checking" from a first page
		// load, and acting on a placeholder is exactly the guess the
		// pluginUnknown branch below exists to refuse.
		switch status := probeChannelPluginStatus(instanceID, channelID); status.State {
		case pluginMissing:
			// the one case worth acting on
		case pluginUnknown:
			log.Printf("plugin-install: %s: cannot confirm %s plugin is absent (%s), not installing",
				name, channelID, status.Detail)
			return
		default:
			// loaded, disabled, or error: the plugin exists. "disabled" in
			// particular must not trigger an install -- it is a config
			// decision (denylist, allowlist, explicit disable) that reinstalling
			// would not change, and the readback already surfaces the reason.
			return
		}

		log.Printf("plugin-install: %s: installing %s for the %s channel", name, spec, channelID)
		res, timedOut := execOpenclawBounded(client, pluginInstallTimeout, "plugins", "install", spec, "--accept-capabilities", "--force")
		switch {
		case timedOut:
			log.Printf("plugin-install: %s: %s install timed out after %s", name, spec, pluginInstallTimeout)
			return
		case res.err != nil:
			log.Printf("plugin-install: %s: %s install failed: %v", name, spec, res.err)
			return
		case res.code != 0:
			log.Printf("plugin-install: %s: %s install exited %d: %s",
				name, spec, res.code, utils.SanitizeForLog(res.stderr))
			return
		}

		// Restart the gateway so the freshly-discovered plugin goes through
		// applyPluginAutoEnable, which is what actually turns it on -- the
		// install alone leaves it discovered but disabled.
		if _, _, _, err := sshproxy.NewSSHInstance(client).ExecOpenclaw(ctx, "gateway", "stop", "--force"); err != nil {
			log.Printf("plugin-install: %s: installed %s but could not restart the gateway: %v", name, spec, err)
			return
		}
		// The cached "missing" is now wrong by construction.
		invalidateChannelPluginStatus(instanceID, channelID)
		log.Printf("plugin-install: %s: installed %s and restarted the gateway", name, spec)
	}()
}
