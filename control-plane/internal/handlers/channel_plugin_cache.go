package handlers

import (
	"fmt"
	"sync"
	"time"
)

// Probing an agent for its plugin state is slow -- see pluginStatusProbeTimeout
// -- so it cannot happen inline in a request. The first version did exactly
// that on an 8s budget and timed out every time, which was worse than useless:
// the settings card learned nothing, and the on-demand installer, which asks
// the same question before acting, read the timeout as "cannot confirm" and
// declined to install.
//
// So the probe moved off the request path. A GET serves the last known answer
// and kicks off a refresh when that answer is stale; the card shows "checking"
// only until the first probe lands. The installer, which is already async,
// still probes synchronously on the full budget -- it can afford to wait, and
// it must have a real answer rather than a placeholder.

type cachedPluginStatus struct {
	status channelPluginStatus
	at     time.Time
}

var (
	pluginStatusMu      sync.Mutex
	pluginStatusCache   = map[string]cachedPluginStatus{}
	pluginStatusRefresh = map[string]bool{}
)

func pluginStatusKey(instanceID uint, channelID string) string {
	return fmt.Sprintf("%d/%s", instanceID, channelID)
}

// channelPluginStatusCached returns the last known plugin state immediately,
// refreshing in the background when it is stale or absent.
//
// A stale-but-real answer beats a placeholder: plugin state changes only when
// something installs, enables or breaks a plugin, so showing the previous
// answer for up to pluginStatusCacheTTL while a new probe runs is honest and
// far more useful than "checking" on every load.
func channelPluginStatusCached(instanceID uint, channelID string) channelPluginStatus {
	key := pluginStatusKey(instanceID, channelID)

	pluginStatusMu.Lock()
	entry, hasEntry := pluginStatusCache[key]
	fresh := hasEntry && time.Since(entry.at) < pluginStatusCacheTTL
	startRefresh := !fresh && !pluginStatusRefresh[key]
	if startRefresh {
		pluginStatusRefresh[key] = true
	}
	pluginStatusMu.Unlock()

	if startRefresh {
		go func() {
			status := probeChannelPluginStatus(instanceID, channelID)
			pluginStatusMu.Lock()
			// A probe that could not reach the agent must not overwrite a real
			// answer with "unknown" -- a brief restart would otherwise erase
			// what we know and leave the card blank until the agent is back.
			// The stale entry is kept and simply re-probed next time.
			if status.State != pluginUnknown || !hasEntry {
				pluginStatusCache[key] = cachedPluginStatus{status: status, at: time.Now()}
			}
			delete(pluginStatusRefresh, key)
			pluginStatusMu.Unlock()
		}()
	}

	if hasEntry {
		return entry.status
	}
	return channelPluginStatus{State: pluginChecking, Detail: "Asking the agent…"}
}

// invalidateChannelPluginStatus drops the cached answer for one channel, so
// the next read re-probes. Called after an install, where the whole point is
// that the previous answer is now wrong.
func invalidateChannelPluginStatus(instanceID uint, channelID string) {
	pluginStatusMu.Lock()
	delete(pluginStatusCache, pluginStatusKey(instanceID, channelID))
	pluginStatusMu.Unlock()
}
