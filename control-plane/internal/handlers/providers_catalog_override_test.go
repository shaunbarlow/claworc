package handlers

import (
	"testing"
)

// TestApplyCatalogOverridesReplacesMatchingEntry asserts a live-feed entry
// whose name matches a hardcoded override is replaced wholesale by the
// pinned entry, while non-overridden entries pass through unchanged.
func TestApplyCatalogOverridesReplacesMatchingEntry(t *testing.T) {
	live := []catalogRootEntry{
		{Name: "anthropic", Label: "Anthropic (stale)", Models: []catalogRootModel{
			{ModelID: "claude-sonnet-4-6"},
		}},
		{Name: "openai", Label: "OpenAI", Models: []catalogRootModel{
			{ModelID: "gpt-5.2"},
		}},
	}
	merged := applyCatalogOverrides(live)
	if len(merged) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(merged))
	}

	var anthropic, openai *catalogRootEntry
	for i := range merged {
		switch merged[i].Name {
		case "anthropic":
			anthropic = &merged[i]
		case "openai":
			openai = &merged[i]
		}
	}
	if anthropic == nil {
		t.Fatal("anthropic entry missing from merged catalog")
	}
	if anthropic.Label != "Anthropic" {
		t.Errorf("expected overridden label %q, got %q (live-feed value leaked through)", "Anthropic", anthropic.Label)
	}
	wantIDs := map[string]bool{
		"claude-opus-5": true, "claude-sonnet-5": true, "claude-haiku-4-5": true,
		"claude-opus-4-8": true, "claude-opus-4-7": true,
	}
	if len(anthropic.Models) != len(wantIDs) {
		t.Fatalf("expected %d overridden anthropic models, got %d: %+v", len(wantIDs), len(anthropic.Models), anthropic.Models)
	}
	for _, m := range anthropic.Models {
		if !wantIDs[m.ModelID] {
			t.Errorf("unexpected model id in overridden anthropic entry: %s", m.ModelID)
		}
		delete(wantIDs, m.ModelID)
	}
	if len(wantIDs) != 0 {
		t.Errorf("overridden anthropic entry is missing model ids: %v", wantIDs)
	}
	// The live feed's stale sonnet-4-6 model id must not survive the override.
	for _, m := range anthropic.Models {
		if m.ModelID == "claude-sonnet-4-6" {
			t.Error("stale live-feed model id claude-sonnet-4-6 leaked through the override")
		}
	}

	if openai == nil {
		t.Fatal("openai entry missing from merged catalog")
	}
	if openai.Label != "OpenAI" || len(openai.Models) != 1 || openai.Models[0].ModelID != "gpt-5.2" {
		t.Errorf("non-overridden openai entry was mutated: %+v", openai)
	}
}

// TestApplyCatalogOverridesAppendsWhenMissing asserts an override provider
// absent from the live feed (e.g. the feed temporarily drops "anthropic")
// still appears in the merged result rather than being silently lost.
func TestApplyCatalogOverridesAppendsWhenMissing(t *testing.T) {
	live := []catalogRootEntry{
		{Name: "openai", Label: "OpenAI"},
	}
	merged := applyCatalogOverrides(live)
	if len(merged) != 2 {
		t.Fatalf("expected live entry + appended override, got %d entries", len(merged))
	}
	found := false
	for _, e := range merged {
		if e.Name == "anthropic" {
			found = true
		}
	}
	if !found {
		t.Error("expected anthropic override to be appended when absent from the live feed")
	}
}

// TestApplyCatalogOverridesNoOverridesConfigured guards the early-return path:
// an empty override map must not touch the input slice at all.
func TestApplyCatalogOverridesNoOverridesConfigured(t *testing.T) {
	orig := hardcodedCatalogOverrides
	hardcodedCatalogOverrides = map[string]catalogRootEntry{}
	defer func() { hardcodedCatalogOverrides = orig }()

	live := []catalogRootEntry{{Name: "anthropic", Label: "Anthropic (untouched)"}}
	merged := applyCatalogOverrides(live)
	if len(merged) != 1 || merged[0].Label != "Anthropic (untouched)" {
		t.Errorf("expected passthrough with no overrides configured, got %+v", merged)
	}
}

// TestHardcodedAnthropicOverrideShape guards the specific model set Shaun
// asked for: latest Opus/Sonnet/Haiku plus the two prior Opus releases.
// This is the test to update the next time Anthropic ships a new model --
// see the comment on hardcodedCatalogOverrides for the "checked" date.
func TestHardcodedAnthropicOverrideShape(t *testing.T) {
	entry, ok := hardcodedCatalogOverrides["anthropic"]
	if !ok {
		t.Fatal("expected a hardcoded anthropic override to exist")
	}
	if entry.APIFormat != "anthropic-messages" {
		t.Errorf("api_format = %q, want anthropic-messages", entry.APIFormat)
	}
	if entry.BaseURL != "https://api.anthropic.com/" {
		t.Errorf("base_url = %q, want https://api.anthropic.com/", entry.BaseURL)
	}

	byID := make(map[string]catalogRootModel, len(entry.Models))
	for _, m := range entry.Models {
		byID[m.ModelID] = m
	}

	cases := []struct {
		id         string
		reasoning  bool
		vision     bool
		input      float64
		output     float64
		cacheRead  float64
		cacheWrite float64
	}{
		{"claude-opus-5", true, true, 5, 25, 0.5, 6.25},
		{"claude-sonnet-5", true, true, 2, 10, 0.2, 2.5},
		{"claude-haiku-4-5", true, true, 1, 5, 0.1, 1.25},
		{"claude-opus-4-8", true, true, 5, 25, 0.5, 6.25},
		{"claude-opus-4-7", true, true, 5, 25, 0.5, 6.25},
	}
	if len(byID) != len(cases) {
		t.Fatalf("expected exactly %d models, got %d: %+v", len(cases), len(byID), byID)
	}
	for _, c := range cases {
		m, ok := byID[c.id]
		if !ok {
			t.Errorf("missing expected model %s", c.id)
			continue
		}
		if m.Reasoning != c.reasoning || m.Vision != c.vision {
			t.Errorf("%s: reasoning/vision = %v/%v, want %v/%v", c.id, m.Reasoning, m.Vision, c.reasoning, c.vision)
		}
		if m.InputCost != c.input || m.OutputCost != c.output {
			t.Errorf("%s: input/output cost = %v/%v, want %v/%v", c.id, m.InputCost, m.OutputCost, c.input, c.output)
		}
		if m.CachedReadCost != c.cacheRead || m.CachedWriteCost != c.cacheWrite {
			t.Errorf("%s: cache read/write cost = %v/%v, want %v/%v", c.id, m.CachedReadCost, m.CachedWriteCost, c.cacheRead, c.cacheWrite)
		}
		if m.ContextWindow == nil {
			t.Errorf("%s: context_window is nil", c.id)
		}
		if m.MaxTokens == nil {
			t.Errorf("%s: max_tokens is nil", c.id)
		}
	}

	// claude-sonnet-5's introductory $2/$10 pricing became permanent (the
	// planned Sept 1 2026 increase to $3/$15 was cancelled) -- guard against
	// a future edit reintroducing the old scheduled-increase price by mistake.
	if sonnet5 := byID["claude-sonnet-5"]; sonnet5.InputCost != 2 || sonnet5.OutputCost != 10 {
		t.Errorf("claude-sonnet-5 pricing = %v/%v, want the now-permanent 2/10 (see Anthropic pricing docs note)", sonnet5.InputCost, sonnet5.OutputCost)
	}
}
