package providers

import (
	"slices"
	"testing"
)

// Copilot embeddings ride api.githubcopilot.com, not the retired GitHub
// Models inference API; the unsupported text-embedding-3-large is replaced
// with text-embedding-ada-002 (decolua/9router#4793).
func TestGithubEmbeddingsPinnedToCopilot(t *testing.T) {
	cfg, ok := KnownProviders["github"]
	if !ok {
		t.Fatal("github provider must be registered")
	}
	if host := cfg.BaseURL; host != "https://api.githubcopilot.com/chat/completions" {
		t.Errorf("github BaseURL = %q, want the Copilot API host", host)
	}
	for _, provider := range []string{"github", "gh"} {
		models := GetProviderModels(provider)
		if slices.Contains(models, "text-embedding-3-large") {
			t.Errorf("%s still lists unsupported text-embedding-3-large", provider)
		}
		for _, want := range []string{"text-embedding-3-small", "text-embedding-ada-002"} {
			if !slices.Contains(models, want) {
				t.Errorf("%s missing %s", provider, want)
			}
		}
	}
	kinds, ok := ProviderModelKinds["github"]
	if !ok {
		t.Fatal("github format map must exist")
	}
	if _, ok := kinds["text-embedding-3-large"]; ok {
		t.Error("github kind map still lists text-embedding-3-large")
	}
	if kind, ok := kinds["text-embedding-ada-002"]; !ok || kind != "embedding" {
		t.Error("github kind map missing text-embedding-ada-002 as embedding")
	}
}
