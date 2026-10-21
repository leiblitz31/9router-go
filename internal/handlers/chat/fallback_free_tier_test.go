package chat

import (
	"net/http"
	"testing"
)

// The two gate shapes below are the real bodies OpenCode returned: the
// inference gate's 403 FreeTierError and the zen endpoint's 429
// FreeUsageLimitError (captured live, 2026-10-21). Both must classify as
// free-tier refusals so neither writes an account lock; lookalikes must not.
func TestIsOpencodeFreeTierRefusal(t *testing.T) {
	zen429 := []byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded. Please try again later."}}`)
	gate403 := []byte(`{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`)

	tests := []struct {
		name     string
		provider string
		ue       *upstreamError
		want     bool
	}{
		{name: "inference gate 403 FreeTierError", provider: "opencode", ue: &upstreamError{StatusCode: http.StatusForbidden, Body: gate403}, want: true},
		{name: "zen 429 FreeUsageLimitError", provider: "opencode", ue: &upstreamError{StatusCode: http.StatusTooManyRequests, Body: zen429}, want: true},
		{name: "zen provider alias 429", provider: "opencode-zen", ue: &upstreamError{StatusCode: http.StatusTooManyRequests, Body: zen429}, want: true},
		{name: "go provider alias 403", provider: "opencode-go", ue: &upstreamError{StatusCode: http.StatusForbidden, Body: gate403}, want: true},
		{name: "429 with a different error type still locks", provider: "opencode", ue: &upstreamError{StatusCode: http.StatusTooManyRequests, Body: []byte(`{"error":{"type":"rate_limit_exceeded"}}`)}, want: false},
		{name: "403 without FreeTierError still locks", provider: "opencode", ue: &upstreamError{StatusCode: http.StatusForbidden, Body: []byte(`{"error":{"type":"invalid_api_key"}}`)}, want: false},
		{name: "FreeUsageLimitError on another status", provider: "opencode", ue: &upstreamError{StatusCode: http.StatusInternalServerError, Body: zen429}, want: false},
		{name: "other provider 429 FreeUsageLimitError", provider: "deepseek", ue: &upstreamError{StatusCode: http.StatusTooManyRequests, Body: zen429}, want: false},
		{name: "other provider 403 FreeTierError", provider: "cline", ue: &upstreamError{StatusCode: http.StatusForbidden, Body: gate403}, want: false},
		{name: "nil error", provider: "opencode", ue: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOpencodeFreeTierRefusal(tt.provider, tt.ue); got != tt.want {
				t.Errorf("isOpencodeFreeTierRefusal(%q, %+v) = %v, want %v", tt.provider, tt.ue, got, tt.want)
			}
		})
	}
}
