package providers

import (
	"strings"
	"testing"
)

// The registry URL for cloudflare-ai always carries the {accountId}
// placeholder: the account id is per-connection state that the chat layer
// resolves from providerSpecificData (CLOUDFLARE_ACCOUNT_ID remains a
// resolution-time fallback there). Baking an env var into the registry
// sent every cf/* request to /accounts//ai/v1/... (Cloudflare 7003)
// whenever the variable was unset.
func TestCloudflareAIRegistryURLPlaceholder(t *testing.T) {
	cfg, ok := KnownProviders["cloudflare-ai"]
	if !ok {
		t.Fatal("cloudflare-ai missing from KnownProviders")
	}
	if !strings.Contains(cfg.BaseURL, "/accounts/{accountId}/ai/v1/chat/completions") {
		t.Errorf("registry BaseURL = %q, want {accountId} placeholder", cfg.BaseURL)
	}
	if strings.Contains(cfg.BaseURL, "/accounts//") {
		t.Errorf("registry BaseURL = %q, must never contain an empty account segment", cfg.BaseURL)
	}
}
