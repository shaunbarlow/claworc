package handlers

import (
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
