package providers

import (
	"slices"
	"testing"
)

// TestGemini4ArgonRegistry is the Go port of upstream's
// tests/unit/gemini-4-argon.test.js (29fc880e): Argon is listed on gemini +
// vertex and the high tier on antigravity, with level-based always-on
// reasoning, 1M context and 64K output.
func TestGemini4ArgonRegistry(t *testing.T) {
	if !slices.Contains(ProviderModels["gemini"], "gemini-4-argon") {
		t.Error("gemini catalog missing gemini-4-argon")
	}
	if !slices.Contains(ProviderModels["vertex"], "gemini-4-argon") {
		t.Error("vertex catalog missing gemini-4-argon")
	}
	if !slices.Contains(ProviderModels["antigravity"], "gemini-4-argon-high") {
		t.Error("antigravity catalog missing gemini-4-argon-high")
	}
	if !slices.Contains(ProviderModels["ag"], "gemini-4-argon-high") {
		t.Error("ag catalog missing gemini-4-argon-high")
	}

	for _, provider := range []string{"gemini", "vertex", "antigravity"} {
		model := "gemini-4-argon"
		if provider == "antigravity" {
			model = "gemini-4-argon-high"
		}
		caps := GetCapabilitiesForModel(provider, model)
		if !caps.Reasoning {
			t.Errorf("%s/%s: Reasoning = false", provider, model)
		}
		if caps.ThinkingFormat != "gemini-level" {
			t.Errorf("%s/%s: ThinkingFormat = %q", provider, model, caps.ThinkingFormat)
		}
		if caps.ThinkingCanDisable == nil || *caps.ThinkingCanDisable {
			t.Errorf("%s/%s: thinking must not be disableable", provider, model)
		}
		if caps.ContextWindow != 1048576 {
			t.Errorf("%s/%s: ContextWindow = %d", provider, model, caps.ContextWindow)
		}
		if caps.MaxOutput != 65536 {
			t.Errorf("%s/%s: MaxOutput = %d", provider, model, caps.MaxOutput)
		}
		if levels := GetThinkingLevels(provider, model); !slices.Equal(levels, []string{"low", "medium", "high"}) {
			t.Errorf("%s/%s: levels = %v, want [low medium high]", provider, model, levels)
		}
	}
}

// TestGemini3KeepsMinimalLevel is the regression guard: Gemini 3 keeps the
// minimal tier that Argon rejects.
func TestGemini3KeepsMinimalLevel(t *testing.T) {
	levels := GetThinkingLevels("gemini", "gemini-3.8-flash")
	if !slices.Contains(levels, "minimal") {
		t.Errorf("gemini-3.8-flash levels = %v, want minimal included", levels)
	}
}
