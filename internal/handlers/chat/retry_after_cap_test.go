package chat

import (
	"net/http"
	"testing"
	"time"
)

// A 429 lock used to be capped at 120s because OpenCode's free tier sent a
// ~2h Retry-After for a window that reopened in minutes, and honouring it
// locked every model on the connection for two hours (observed live,
// 2026-10-21). Upstream's retry rework replaced that fork cap with a
// per-provider ceiling: long waits are now honoured up to maxResetCooldown
// (2h), with a 7d exception for deepseek-web whose mute window is genuinely
// that long. Short waits are still honoured exactly, and non-429 statuses
// keep the old reading. This test pins the merged behaviour, not the old cap.
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
		provider   string
		want       int
	}{
		{name: "429 with a two-hour Retry-After is honoured up to the 2h cap", statusCode: 429, base: 2 * time.Second, ue: withRetryAfter("7200"), provider: "", want: 7200},
		{name: "429 with a short Retry-After is honoured", statusCode: 429, base: 2 * time.Second, ue: withRetryAfter("30"), provider: "", want: 30},
		{name: "429 with a 120s Retry-After is honoured", statusCode: 429, base: 2 * time.Second, ue: withRetryAfter("120"), provider: "", want: 120},
		{name: "429 without Retry-After keeps a short classifier base", statusCode: 429, base: 8 * time.Second, ue: &upstreamError{StatusCode: 429}, provider: "", want: 8},
		{name: "429 without Retry-After keeps a longer classifier base up to the 2h cap", statusCode: 429, base: 300 * time.Second, ue: &upstreamError{StatusCode: 429}, provider: "", want: 300},
		{name: "non-429 keeps its long cooldown", statusCode: 403, base: 3600 * time.Second, ue: &upstreamError{StatusCode: 403}, provider: "", want: 3600},
		{name: "non-429 is still bounded by maxResetCooldown", statusCode: 403, base: 3 * time.Hour, ue: &upstreamError{StatusCode: 403}, provider: "", want: 7200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryableCooldownSec(tt.statusCode, tt.base, tt.ue, tt.provider); got != tt.want {
				t.Errorf("retryableCooldownSec(%d, %v, ue, %q) = %d, want %d", tt.statusCode, tt.base, tt.provider, got, tt.want)
			}
		})
	}
}
