package providers

import (
	"slices"
	"testing"
)

// Upstream 7878f53b: gpt-5.5 is retired from Codex with ChatGPT sign-in
// (2026-10-14) and the backend answers HTTP 400, so the registry entries and
// the image variant must not be published on the Codex lane.
func TestCodexCatalogDropsRetiredGPT55(t *testing.T) {
	for _, provider := range []string{"codex", "cx"} {
		for _, model := range ProviderModels[provider] {
			switch model {
			case "gpt-5.5", "gpt-5.5-review", "gpt-5.5-image":
				t.Errorf("%s still publishes retired %q", provider, model)
			}
		}
		if !slices.Contains(ProviderModels[provider], "gpt-6.1-sol") {
			t.Errorf("%s is missing the CLI preset default gpt-6.1-sol", provider)
		}
		if !slices.Contains(ProviderModels[provider], "gpt-5.6-sol") {
			t.Errorf("%s is missing gpt-5.6-sol, the image lane's new main model", provider)
		}
	}
}

// Other providers that legitimately serve gpt-5.5 are untouched.
func TestGPT55SurvivesOnOtherProviders(t *testing.T) {
	for _, provider := range []string{"openai", "llm7", "bazaarlink", "bzl", "blackbox", "bb"} {
		if !slices.Contains(ProviderModels[provider], "gpt-5.5") {
			t.Errorf("%s lost gpt-5.5, but only the Codex lane retired it", provider)
		}
	}
	if !slices.Contains(ProviderModels["opencode-zen"], "gpt-5.5") {
		t.Error("opencode-zen lost gpt-5.5, but only the Codex lane retired it")
	}
}

// Upstream #4770: the Nous provider is registered with its Portal base URL and
// a bearer scheme, reachable through its three aliases.
func TestNousProviderRegistered(t *testing.T) {
	cfg, ok := KnownProviders["nous"]
	if !ok {
		t.Fatal("nous missing from KnownProviders")
	}
	if cfg.BaseURL != "https://inference-api.nousresearch.com/v1/chat/completions" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.AuthHeader != "Authorization" || cfg.AuthScheme != "bearer" {
		t.Errorf("auth = %q/%q, want Authorization/bearer", cfg.AuthHeader, cfg.AuthScheme)
	}
	for _, alias := range []string{"nous", "nous-portal", "nous-research", "ns"} {
		if got := ResolveAlias(alias); got != "nous" {
			t.Errorf("ResolveAlias(%q) = %q, want nous", alias, got)
		}
	}
}
