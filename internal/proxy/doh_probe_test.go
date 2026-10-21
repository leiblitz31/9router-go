package proxy

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// TestProbeDoHResolvers exercises the dashboard pool-test probe against a
// fake DoH endpoint. The exported wrapper builds its resolver with the
// production (SSRF-safe) client, which refuses the test server's loopback
// address, so the success paths drive the same probe core through a
// resolver whose client was swapped for a plain one — the pattern
// newTestDoHResolver establishes for the resolver itself.
func TestProbeDoHResolvers(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{
			name: "healthy resolver answers",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("name"); got != "cloudflare.com" {
					t.Errorf("query name = %q, want cloudflare.com", got)
				}
				w.Header().Set("Content-Type", "application/dns-json")
				fmt.Fprint(w, `{"Status":0,"Answer":[{"name":"cloudflare.com","type":1,"TTL":300,"data":"104.16.132.229"}]}`)
			},
			wantErr: false,
		},
		{
			name: "private answer fails the probe",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/dns-json")
				fmt.Fprint(w, `{"Status":0,"Answer":[{"name":"cloudflare.com","type":1,"TTL":300,"data":"192.168.1.1"}]}`)
			},
			wantErr: true,
		},
		{
			name: "resolver error fails the probe",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver, _ := newTestDoHResolver(t, tt.handler)
			latencyMs, err := probeDoHResolvers(context.Background(), resolver, "cloudflare.com")
			if (err != nil) != tt.wantErr {
				t.Fatalf("probeDoHResolvers() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && latencyMs < 0 {
				t.Errorf("latency = %d, want >= 0", latencyMs)
			}
		})
	}
}

// TestProbeDoHResolversBadSpecs covers the exported wrapper end to end for
// specs that can never answer: neither case needs the network to fail.
func TestProbeDoHResolversBadSpecs(t *testing.T) {
	tests := []struct {
		name  string
		specs []string
	}{
		{name: "spec that is not a URL", specs: []string{"not a url"}},
		{name: "unreachable endpoint", specs: []string{"http://127.0.0.1:1/dns-query"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ProbeDoHResolvers(context.Background(), tt.specs, "cloudflare.com"); err == nil {
				t.Errorf("ProbeDoHResolvers(%v) = nil error, want failure", tt.specs)
			}
		})
	}
}
