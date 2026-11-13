package executor

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestQwenWebMergeCookieHeader(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		set      []*http.Cookie
		expected string
	}{
		{
			name:     "no set cookies returns input",
			existing: "a=1; b=2",
			set:      nil,
			expected: "a=1; b=2",
		},
		{
			name:     "rotated value replaces in place",
			existing: "a=1; refresh_token=old; b=2",
			set:      []*http.Cookie{{Name: "refresh_token", Value: "new"}},
			expected: "a=1; refresh_token=new; b=2",
		},
		{
			name:     "new name is appended",
			existing: "a=1",
			set:      []*http.Cookie{{Name: "auth-session", Value: "s3"}},
			expected: "a=1; auth-session=s3",
		},
		{
			name:     "same value leaves input untouched",
			existing: "a=1; b=2",
			set:      []*http.Cookie{{Name: "b", Value: "2"}},
			expected: "a=1; b=2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qwenWebMergeCookieHeader(tt.existing, tt.set); got != tt.expected {
				t.Errorf("qwenWebMergeCookieHeader() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestQwenWebMintHeaders(t *testing.T) {
	// Seed the umid cache so the header build stays off the network.
	qwenWebUMIDCache.mu.Lock()
	qwenWebUMIDCache.token = "umid-test"
	qwenWebUMIDCache.expiry = time.Now().Add(time.Hour)
	qwenWebUMIDCache.mu.Unlock()

	h := qwenWebMintHeaders(context.Background(), nil)
	if got := h.Get("x-request-origin"); got != qwenWebBaseURL {
		t.Errorf("x-request-origin = %q, want %q", got, qwenWebBaseURL)
	}
	if got := h.Get("Sec-Fetch-Site"); got != "same-site" {
		t.Errorf("Sec-Fetch-Site = %q, want same-site", got)
	}
	if got := h.Get("bx-umidtoken"); got != "umid-test" {
		t.Errorf("bx-umidtoken = %q, want umid-test", got)
	}
	if got := h.Get("bx-ua"); got == "" {
		t.Error("bx-ua is empty, want the synthetic fingerprint")
	}
	if got := h.Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q, want absent on the bodyless refresh GET", got)
	}
}
