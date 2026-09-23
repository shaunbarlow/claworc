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

// audioTranscriptionEnabled is deliberately a global, opt-in capability. It
// only renders static OpenClaw config; it does not install a plugin, launch a
// sidecar, probe a provider, or add work to the agent startup path.
func audioTranscriptionEnabled() bool {
	v, err := database.GetSetting("default_audio_transcription")
	return err == nil && v == "true"
}

// managedAudioTranscriptionConfig is the minimal OpenClaw media configuration
// required for inbound voice-message/audio-attachment transcription. Claworc's
// OpenAI provider is already routed through its per-agent LLM gateway, so no
// extra credentials or process are introduced here.
func managedAudioTranscriptionConfig() map[string]interface{} {
	return map[string]interface{}{
		"models": []map[string]interface{}{{
			"type":         "provider",
			"provider":     "openai",
			"model":        "gpt-4o-transcribe",
			"capabilities": []string{"audio"},
		}},
		"audio": map[string]interface{}{"enabled": true},
	}
}

// applyAudioTranscriptionConfig changes a hot-reloadable OpenClaw config path.
// It intentionally never restarts the container or gateway: OpenClaw applies
// tools.media changes live. Disabling removes only Claworc's managed subtree.
func applyAudioTranscriptionConfig(ctx context.Context, agent sshproxy.Instance, name string, enabled bool, providerKeys []string) {
	name = utils.SanitizeForLog(name)
	if !enabled {
		if _, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "unset", "tools.media"); err != nil || code != 0 {
			log.Printf("audio-transcription: %s: unset tools.media failed: %v stderr=%q", name, err, utils.SanitizeForLog(stderr))
		}
		return
	}
	// Existing agents can predate the provider-level policy emitted during
	// provisioning. Write the narrow exception explicitly before enabling
	// media, so the first voice note cannot fail with SsrFBlockedError.
	for _, providerKey := range providerKeys {
		path := "models.providers." + providerKey + ".request.allowPrivateNetwork"
		if _, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "set", path, "true", "--strict-json"); err != nil || code != 0 {
			log.Printf("audio-transcription: %s: set %s failed: %v stderr=%q", name, path, err, utils.SanitizeForLog(stderr))
		}
	}

	payload, err := json.Marshal(managedAudioTranscriptionConfig())
	if err != nil {
		log.Printf("audio-transcription: %s: marshal config: %v", name, err)
		return
	}
	if _, stderr, code, err := agent.ExecOpenclaw(ctx, "config", "set", "tools.media", string(payload), "--replace", "--json"); err != nil || code != 0 {
		log.Printf("audio-transcription: %s: set tools.media failed: %v stderr=%q", name, err, utils.SanitizeForLog(stderr))
	}
}

func pushAudioTranscriptionConfigForRunningInstances() {
	if SSHMgr == nil {
		return
	}
	enabled := audioTranscriptionEnabled()
	var running []database.Instance
	database.DB.Where("status = ?", "running").Find(&running)
	for i := range running {
		if database.IsLegacyEmbedded(running[i].ContainerImage) {
			continue
		}
		id, name := running[i].ID, running[i].Name
		providerKeys := make([]string, 0)
		for key := range resolveGatewayProviders(running[i]) {
			providerKeys = append(providerKeys, key)
		}
		go func() {
			ctx := context.Background()
			client, err := SSHMgr.WaitForSSH(ctx, id, 120*time.Second)
			if err != nil {
				log.Printf("audio-transcription: no SSH connection for instance %d: %v", id, err)
				return
			}
			applyAudioTranscriptionConfig(ctx, sshproxy.NewSSHInstance(client), name, enabled, providerKeys)
		}()
	}
}
