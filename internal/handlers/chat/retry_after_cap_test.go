package chat

import (
	"net/http"
	"testing"
	"time"
)

// A 429 lock must never park an account for the upstream's full named
// window: OpenCode's free tier sends a ~2h Retry-After for a window that
// reopens in minutes, and honouring it locked every model on the
// connection for two hours (observed live, 2026-10-21). Short waits are
// still honoured exactly, and non-429 statuses keep the old reading.
func TestRetryableCooldownSec429Cap(t *testing.T) {
	withRetryAfter := func(seconds string) *upstreamError {
		return &upstreamError{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": {seconds}},
		}
	}

	tests := []struct {
		name       string
		statusCode int
		base       time.Duration
		ue         *upstreamError
		want       int
	}{
		{name: "429 with a two-hour Retry-After is capped at 120s", statusCode: 429, base: 2 * time.Second, ue: withRetryAfter("7200"), want: 120},
		{name: "429 with a short Retry-After is honoured", statusCode: 429, base: 2 * time.Second, ue: withRetryAfter("30"), want: 30},
		{name: "429 with a Retry-After at the cap", statusCode: 429, base: 2 * time.Second, ue: withRetryAfter("120"), want: 120},
		{name: "429 without Retry-After keeps a short classifier base", statusCode: 429, base: 8 * time.Second, ue: &upstreamError{StatusCode: 429}, want: 8},
		{name: "429 without Retry-After caps a maxed classifier base", statusCode: 429, base: 300 * time.Second, ue: &upstreamError{StatusCode: 429}, want: 120},
		{name: "non-429 keeps its long cooldown", statusCode: 403, base: 3600 * time.Second, ue: &upstreamError{StatusCode: 403}, want: 3600},
		{name: "non-429 is still bounded by maxResetCooldown", statusCode: 403, base: 3 * time.Hour, ue: &upstreamError{StatusCode: 403}, want: 7200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryableCooldownSec(tt.statusCode, tt.base, tt.ue); got != tt.want {
				t.Errorf("retryableCooldownSec(%d, %v, ue) = %d, want %d", tt.statusCode, tt.base, got, tt.want)
			}
		})
	}
}
