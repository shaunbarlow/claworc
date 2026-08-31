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
		"claude-opus-4-8": true, "claude-opus-4-7": true, "claude-sonnet-4-6": true,
	}
	if len(anthropic.Models) != len(wantIDs) {
		t.Fatalf("expected %d overridden anthropic models, got %d: %+v", len(wantIDs), len(anthropic.Models), anthropic.Models)
	}
	var sonnet46 *catalogRootModel
	for i := range anthropic.Models {
		m := &anthropic.Models[i]
		if !wantIDs[m.ModelID] {
			t.Errorf("unexpected model id in overridden anthropic entry: %s", m.ModelID)
		}
		delete(wantIDs, m.ModelID)
		if m.ModelID == "claude-sonnet-4-6" {
			sonnet46 = m
		}
	}
	if len(wantIDs) != 0 {
		t.Errorf("overridden anthropic entry is missing model ids: %v", wantIDs)
	}
	// claude-sonnet-4-6 is a legitimate pinned fallback entry (added
	// 2026-08-31), not the live feed's bare stale stub -- assert it carries
	// real pinned field values rather than the empty stub's zero values.
	if sonnet46 == nil {
		t.Fatal("expected claude-sonnet-4-6 fallback entry in overridden anthropic models")
	}
	if sonnet46.InputCost != 3 || sonnet46.OutputCost != 15 {
		t.Errorf("claude-sonnet-4-6 pricing = %v/%v, want the pinned 3/15 (the live feed's stale stub must not leak through)", sonnet46.InputCost, sonnet46.OutputCost)
	}

	if openai == nil {
		t.Fatal("openai entry missing from merged catalog")
	}
	if openai.Label != "OpenAI" {
		t.Errorf("expected overridden label %q, got %q (live-feed value leaked through)", "OpenAI", openai.Label)
	}
	wantOpenAIIDs := map[string]bool{
		"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
		"gpt-5.5": true, "gpt-5.4": true,
	}
	if len(openai.Models) != len(wantOpenAIIDs) {
		t.Fatalf("expected %d overridden openai models, got %d: %+v", len(wantOpenAIIDs), len(openai.Models), openai.Models)
	}
	for _, m := range openai.Models {
		if !wantOpenAIIDs[m.ModelID] {
			t.Errorf("unexpected model id in overridden openai entry: %s", m.ModelID)
		}
		delete(wantOpenAIIDs, m.ModelID)
		if m.ModelID == "gpt-5.2" {
			t.Error("stale live-feed model id gpt-5.2 leaked through the override")
		}
	}
	if len(wantOpenAIIDs) != 0 {
		t.Errorf("overridden openai entry is missing model ids: %v", wantOpenAIIDs)
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
// asked for: latest Opus/Sonnet/Haiku, the two prior Opus releases, and a
// prior-generation Sonnet fallback (claude-sonnet-4-6). This is the test to
// update the next time Anthropic ships a new model -- see the comment on
// hardcodedCatalogOverrides for the "checked" date.
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
		{"claude-sonnet-4-6", true, true, 3, 15, 0.3, 3.75},
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

// TestHardcodedOpenAIOverrideShape guards the OpenAI model set Shaun asked
// for: the current GPT-5.6 family (Sol/Terra/Luna) plus the two prior
// flagship generations (GPT-5.5, GPT-5.4) kept for compatibility. This is
// the test to update the next time OpenAI ships a new model -- see the
// comment on hardcodedCatalogOverrides for the "checked" date.
func TestHardcodedOpenAIOverrideShape(t *testing.T) {
	entry, ok := hardcodedCatalogOverrides["openai"]
	if !ok {
		t.Fatal("expected a hardcoded openai override to exist")
	}
	if entry.APIFormat != "openai-completions" {
		t.Errorf("api_format = %q, want openai-completions", entry.APIFormat)
	}
	if entry.BaseURL != "https://api.openai.com/" {
		t.Errorf("base_url = %q, want https://api.openai.com/", entry.BaseURL)
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
		{"gpt-5.6-sol", true, true, 4, 20, 0.4, 5},
		{"gpt-5.6-terra", true, true, 2, 12, 0.2, 2.5},
		{"gpt-5.6-luna", true, true, 0.2, 1.2, 0.02, 0.25},
		{"gpt-5.5", true, true, 5, 30, 0.5, 0},
		{"gpt-5.4", true, true, 2.5, 15, 0.25, 0},
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
}
