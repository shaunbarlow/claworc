package handlers

import (
	"context"
	"encoding/json"
	"testing"
)

func TestManagedAudioTranscriptionConfig(t *testing.T) {
	cfg := managedAudioTranscriptionConfig()
	if cfg["audio"].(map[string]interface{})["enabled"] != true {
		t.Fatal("audio transcription must be enabled")
	}
	models := cfg["models"].([]map[string]interface{})
	if len(models) != 1 || models[0]["provider"] != "openai" || models[0]["model"] != "gpt-4o-transcribe" {
		t.Fatalf("unexpected audio models: %#v", models)
	}
	if _, err := json.Marshal(cfg); err != nil {
		t.Fatalf("config must be JSON serializable: %v", err)
	}
}

func TestBuildOpenClawConfigBatchIncludesAudioWithoutExtraBootstrapCommand(t *testing.T) {
	batch, err := buildOpenClawConfigBatch(nil, "", true)
	if err != nil { t.Fatal(err) }
	var ops []openclawConfigBatchOperation
	if err := json.Unmarshal([]byte(batch), &ops); err != nil { t.Fatal(err) }
	if len(ops) != 1 || ops[0].Path != "tools.media" {
		t.Fatalf("audio config must be part of the initial batch: %#v", ops)
	}
}

func TestApplyAudioTranscriptionConfigSetsProviderPrivateNetworkPolicyFirst(t *testing.T) {
	inst := &mockInstance{}
	applyAudioTranscriptionConfig(context.Background(), inst, "test", true, []string{"openai"})
	if len(inst.calls) != 2 {
		t.Fatalf("calls = %#v, want provider policy plus media config", inst.calls)
	}
	if got := inst.calls[0]; len(got) != 5 || got[0] != "config" || got[1] != "set" || got[2] != "models.providers.openai.request.allowPrivateNetwork" || got[3] != "true" || got[4] != "--strict-json" {
		t.Errorf("provider policy call = %#v", got)
	}
	if got := inst.calls[1]; len(got) < 4 || got[0] != "config" || got[1] != "set" || got[2] != "tools.media" {
		t.Errorf("media config call = %#v", got)
	}
}
