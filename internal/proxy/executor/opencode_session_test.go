package executor

import (
	"testing"

	"9router/proxy/internal/proxy"
)

func TestOpencodeSessionHint(t *testing.T) {
	tests := []struct {
		name     string
		req      *Request
		free     bool
		wantKey  string // stable-session key the hint must resolve to (free lane)
		wantHint string // exact hint (keyed lane)
	}{
		{
			name:     "free lane ignores a client session",
			req:      &Request{ConnectionID: "conn-1", SessionID: "ses_client"},
			free:     true,
			wantKey:  "opencode:conn:conn-1",
			wantHint: "",
		},
		{
			name:     "free lane without a client session uses the stable session",
			req:      &Request{ConnectionID: "conn-1"},
			free:     true,
			wantKey:  "opencode:conn:conn-1",
			wantHint: "",
		},
		{
			name:     "free lane without a connection falls back to the default key",
			req:      &Request{},
			free:     true,
			wantKey:  "opencode:default",
			wantHint: "",
		},
		{
			name:     "keyed lane keeps the client session",
			req:      &Request{ConnectionID: "conn-1", SessionID: "ses_client"},
			free:     false,
			wantKey:  "",
			wantHint: "ses_client",
		},
		{
			name:     "keyed lane without a client session has no hint",
			req:      &Request{ConnectionID: "conn-1"},
			free:     false,
			wantKey:  "",
			wantHint: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := opencodeSessionHint(tt.req, tt.free)
			if tt.free {
				want := proxy.StableOpenCodeSessionID(tt.wantKey)
				if got != want {
					t.Errorf("opencodeSessionHint() = %q, want stable session %q", got, want)
				}
				if tt.req.SessionID != "" && got == tt.req.SessionID {
					t.Errorf("opencodeSessionHint() leaked the client session %q", got)
				}
				return
			}
			if got != tt.wantHint {
				t.Errorf("opencodeSessionHint() = %q, want %q", got, tt.wantHint)
			}
		})
	}
}
