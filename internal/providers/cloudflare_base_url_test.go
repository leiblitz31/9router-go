package providers

import (
	"strings"
	"testing"
)

// The registry URL for cloudflare-ai must never carry an empty account
// segment: an explicit CLOUDFLARE_ACCOUNT_ID wins, otherwise the {accountId}
// placeholder stays for the chat layer to fill per connection.
func TestCloudflareAIBaseURL(t *testing.T) {
	t.Run("env unset keeps placeholder", func(t *testing.T) {
		t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
		got := cloudflareAIBaseURL()
		if !strings.Contains(got, "/accounts/{accountId}/ai/v1/chat/completions") {
			t.Errorf("cloudflareAIBaseURL() = %q, want {accountId} placeholder", got)
		}
	})

	t.Run("env set wins", func(t *testing.T) {
		t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acc999")
		got := cloudflareAIBaseURL()
		if !strings.Contains(got, "/accounts/acc999/ai/v1/chat/completions") {
			t.Errorf("cloudflareAIBaseURL() = %q, want env account id", got)
		}
	})
}
