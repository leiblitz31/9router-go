package providers

import "testing"

// Upstream 4c58e7f3: an OpenCode Free id present in no table and no pattern
// falls through to DefaultCapabilities (reasoning: false), so the client's
// reasoning_effort is stripped before the request reaches upstream (#2690).

func TestOpencodeFreeReasoningEntries(t *testing.T) {
	for _, id := range []string{
		"space-bunny-free", "space-bunny", "big-pickle", "exo-free",
		"fledge-alpha-free", "longcat-2.0", "longcat-2.0-free",
		"longcat-2.5-preview-free", "north-mini-code-free",
		"ring-2.6-1t-free", "x-preview-f-free",
	} {
		InvalidateCapabilitiesCache()
		c := GetCapabilitiesForModel("opencode", id)
		if !c.Reasoning {
			t.Errorf("%s: Reasoning=false, want true (reasoning_effort would be stripped)", id)
		}
		if c.ThinkingFormat != "openai" {
			t.Errorf("%s: ThinkingFormat = %q, want openai", id, c.ThinkingFormat)
		}
		if c.ContextWindow <= 0 || c.MaxOutput <= 0 {
			t.Errorf("%s: limits %d/%d, want both declared (verbatim from models.dev)", id, c.ContextWindow, c.MaxOutput)
		}
	}
}

func TestOpencodeFreeReasoningPerProvider(t *testing.T) {
	for _, provider := range []string{"opencode", "oc", "opencode-zen", "opencode-go", "ocz"} {
		InvalidateCapabilitiesCache()
		c := GetCapabilitiesForModel(provider, "space-bunny-free")
		if !c.Reasoning || c.ThinkingFormat != "openai" {
			t.Errorf("%s/space-bunny-free: reasoning=%v format=%q, want true/openai", provider, c.Reasoning, c.ThinkingFormat)
		}
	}
}

// Upstream #4727: claude-haiku-5-5 on opencode-go overrides the global
// *claude-haiku* pattern (which would send thinking.type.enabled → 400) and
// pins the catalog limits, because a provider override short-circuits before
// the catalog overlay.
func TestOpencodeGoHaiku55Overrides(t *testing.T) {
	InvalidateCapabilitiesCache()
	c := GetCapabilitiesForModel("opencode-go", "claude-haiku-5-5")
	if c.ThinkingFormat != "claude-adaptive" {
		t.Errorf("ThinkingFormat = %q, want claude-adaptive", c.ThinkingFormat)
	}
	if c.ContextWindow != 1000000 || c.MaxOutput != 128000 {
		t.Errorf("limits %d/%d, want 1000000/128000", c.ContextWindow, c.MaxOutput)
	}
	if !c.Reasoning || !c.Vision || !c.Search {
		t.Errorf("modalities: reasoning=%v vision=%v search=%v, want all true", c.Reasoning, c.Vision, c.Search)
	}

	// The global pattern (any provider) must not claim adaptive for this id.
	InvalidateCapabilitiesCache()
	if g := GetCapabilitiesForModel("claude", "claude-haiku-5-5"); g.ThinkingFormat == "claude-adaptive" {
		t.Error("the global claude pattern must not be overridden by the opencode-go entry")
	}
}

// The model must be listed so /v1/models publishes it on the opencode-go lane.
func TestOpencodeGoCatalogHasHaiku55(t *testing.T) {
	found := false
	for _, m := range ProviderModels["opencode-go"] {
		if m == "claude-haiku-5-5" {
			found = true
			break
		}
	}
	if !found {
		t.Error("claude-haiku-5-5 missing from the opencode-go catalog")
	}
}
