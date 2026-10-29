package chat

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"9router/proxy/internal/proxy/executor"
)

// TestGetClientForConnection_OpencodeFreeLaneBunHelloDirect pins the last
// path where the OpenCode free lane did not present Bun's ClientHello: a
// connection with no pool and no proxy fell through to the plain client,
// whose Chrome list the free-tier JA4 gate refuses. That is the Go side of
// the decolua/9router#5310 principle (the TLS fingerprint must ride every
// path): the DoH path got Bun in 2ec1e311 and the HTTP-proxy path in
// a741dcaf; this completes the direct path. Other providers keep the plain
// client untouched.
func TestGetClientForConnection_OpencodeFreeLaneBunHelloDirect(t *testing.T) {
	bunSpec, err := executor.OpencodeBunSpecFactory()()
	if err != nil {
		t.Fatalf("bun spec: %v", err)
	}

	tests := []struct {
		name     string
		provider string
		want     []uint16
	}{
		{
			name:     "opencode free lane sends Bun hello on direct path",
			provider: "opencode",
			want:     bunSpec.CipherSuites,
		},
		{
			name:     "oc alias sends Bun hello on direct path",
			provider: "oc",
			want:     bunSpec.CipherSuites,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := newCaptureServer(t)

			h := &ChatHandler{Client: &http.Client{}}
			client, err := h.getClientForConnection(&ConnectionData{}, tt.provider)
			if err != nil {
				t.Fatalf("getClientForConnection: %v", err)
			}
			if client == h.Client {
				t.Fatalf("expected a dedicated Bun client for %q, got the plain client", tt.provider)
			}
			client.Timeout = 10 * time.Second
			// The handshake never completes by design; only the hello matters.
			resp, err := client.Get("https://" + target.ln.Addr().String())
			if err == nil {
				resp.Body.Close()
			}
			for _, got := range target.wait(t) {
				// The wire GREASE slot is randomized per handshake, so
				// compare GREASE-stripped lists; when the spec carries a
				// GREASE placeholder the wire hello must carry a GREASE
				// value in its place.
				if !reflect.DeepEqual(stripGREASE(got), stripGREASE(tt.want)) {
					t.Errorf("ClientHello ciphers = %v, want %v (GREASE-stripped)", stripGREASE(got), stripGREASE(tt.want))
				}
				wantGREASE := false
				for _, c := range tt.want {
					if isGREASE(c) {
						wantGREASE = true
					}
				}
				if wantGREASE {
					if len(got) == 0 || !isGREASE(got[0]) {
						t.Errorf("ClientHello first cipher missing GREASE: got %v", got)
					}
				}
			}
		})
	}

	t.Run("other provider keeps plain client on direct path", func(t *testing.T) {
		h := &ChatHandler{Client: &http.Client{}}
		client, err := h.getClientForConnection(&ConnectionData{}, "openai")
		if err != nil {
			t.Fatalf("getClientForConnection: %v", err)
		}
		if client != h.Client {
			t.Errorf("expected the plain client for openai, got a dedicated client")
		}
	})
}
