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

func TestOpenCodeFreeModelCaps(t *testing.T) {
	tests := []struct {
		model string
		cw    int
		max   int
	}{
		{"longcat-2.5-free", 1000000, 262144},
		{"iceman-free", 262144, 131072},
		{"inkling-2-small-free", 1048576, 131072},
		{"beam-free", 1048576, 131072},
		{"flint-40b-free", 262144, 65536},
		{"nemotron-3.5-super-vl-free", 1048576, 131072},
		{"jev-1.13-free", 64000, 32000},
		{"nemotron-3.5-lightning-free", 262144, 262144},
		{"hy4-flash-free", 1048576, 65536},
		{"tidepool-alpha-free", 1048576, 131072},
	}
	for _, p := range []string{"opencode", "oc"} {
		for _, tt := range tests {
			t.Run(p+"/"+tt.model, func(t *testing.T) {
				caps := GetCapabilitiesForModel(p, tt.model)
				if caps.ContextWindow != tt.cw {
					t.Errorf("context window = %d, want %d", caps.ContextWindow, tt.cw)
				}
				if caps.MaxOutput != tt.max {
					t.Errorf("max output = %d, want %d", caps.MaxOutput, tt.max)
				}
				if caps.ContextWindow == 128000 && caps.MaxOutput == 4096 {
					t.Errorf("hit 128k floor")
				}
			})
		}
	}
}

func TestOpenCodeFreeModelCapsDetailLanes(t *testing.T) {
	for _, p := range []string{"opencode", "oc", "opencode-go", "opencode-zen", "ocz"} {
		t.Run(p, func(t *testing.T) {
			if p == "opencode-go" {
				caps := GetCapabilitiesDetailForModel(p, "beam-free")
				if caps.MaxOutput != 131072 {
					t.Errorf("beam-free max output = %d, want 131072", caps.MaxOutput)
				}
			}
			if p == "opencode-zen" || p == "ocz" {
				caps := GetCapabilitiesDetailForModel(p, "nemotron-3.5-lightning-free")
				if caps.MaxOutput != 262144 {
					t.Errorf("nemotron-3.5-lightning-free max output = %d, want 262144", caps.MaxOutput)
				}
			}
		})
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

	// The global row is upstream's now (Anthropic's Haiku 5.5 takes adaptive), so
	// this test only pins that the provider-scoped override still applies.
	InvalidateCapabilitiesCache()
	if g := GetCapabilitiesForModel("opencode-go", "claude-haiku-5-5"); g.ThinkingFormat != "claude-adaptive" {
		t.Errorf("opencode-go lane lost its override: %q", g.ThinkingFormat)
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