package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoHEndpoint(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want string
	}{
		{name: "cloudflare alias", spec: "cloudflare", want: "https://cloudflare-dns.com/dns-query"},
		{name: "google alias", spec: "google", want: "https://dns.google/resolve"},
		{name: "quad9 alias", spec: "quad9", want: "https://dns.quad9.net/dns-query"},
		{name: "adguard alias", spec: "adguard", want: "https://dns.adguard-dns.com/dns-query"},
		{name: "alias is case-insensitive", spec: "Cloudflare", want: "https://cloudflare-dns.com/dns-query"},
		{name: "alias trims space", spec: "  google ", want: "https://dns.google/resolve"},
		{name: "endpoint URL passes through", spec: "https://one.one.one.one/dns-query", want: "https://one.one.one.one/dns-query"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dohEndpoint(tt.spec); got != tt.want {
				t.Errorf("dohEndpoint(%q) = %q, want %q", tt.spec, got, tt.want)
			}
		})
	}
}

func TestParseDoHResponse(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantIPs     []string
		wantTTL     time.Duration
		wantErr     bool
		wantErrText string
	}{
		{
			name:    "two public A records keep the smallest TTL",
			body:    `{"Status":0,"Answer":[{"name":"example.com","type":1,"TTL":300,"data":"93.184.216.34"},{"name":"example.com","type":1,"TTL":120,"data":"93.184.216.35"}]}`,
			wantIPs: []string{"93.184.216.34", "93.184.216.35"},
			wantTTL: 120 * time.Second,
		},
		{
			name:    "private answer is rejected",
			body:    `{"Status":0,"Answer":[{"name":"example.com","type":1,"TTL":300,"data":"192.168.1.1"}]}`,
			wantErr: true,
		},
		{
			name:    "loopback answer is rejected",
			body:    `{"Status":0,"Answer":[{"name":"example.com","type":1,"TTL":300,"data":"127.0.0.1"}]}`,
			wantErr: true,
		},
		{
			name:    "mixed answer keeps only the public record",
			body:    `{"Status":0,"Answer":[{"name":"example.com","type":1,"TTL":300,"data":"10.0.0.1"},{"name":"example.com","type":1,"TTL":300,"data":"8.8.8.8"}]}`,
			wantIPs: []string{"8.8.8.8"},
			wantTTL: 300 * time.Second,
		},
		{
			name:    "CNAME-only answer has no dial target",
			body:    `{"Status":0,"Answer":[{"name":"example.com","type":5,"TTL":300,"data":"target.example.com"}]}`,
			wantErr: true,
		},
		{
			name:    "empty answer is an error",
			body:    `{"Status":3}`,
			wantErr: true,
		},
		{
			name:        "sinkholed answer names the reserved address",
			body:        `{"Status":0,"Answer":[{"name":"www.codebuddy.ai","type":5,"TTL":60,"data":"www.codebuddy.ai.eo.dnse2.com."},{"name":"www.codebuddy.ai","type":1,"TTL":60,"data":"0.0.0.1"}]}`,
			wantErr:     true,
			wantErrText: "reserved addresses (0.0.0.1)",
		},
		{
			name:    "malformed JSON is an error",
			body:    `not json`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ips, ttl, err := parseDoHResponse([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseDoHResponse() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.wantErrText != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErrText)) {
					t.Fatalf("parseDoHResponse() error = %v, want it to mention %q", err, tt.wantErrText)
				}
				return
			}
			if len(ips) != len(tt.wantIPs) {
				t.Fatalf("parseDoHResponse() ips = %v, want %v", ips, tt.wantIPs)
			}
			for i := range ips {
				if ips[i] != tt.wantIPs[i] {
					t.Errorf("ips[%d] = %q, want %q", i, ips[i], tt.wantIPs[i])
				}
			}
			if ttl != tt.wantTTL {
				t.Errorf("ttl = %v, want %v", ttl, tt.wantTTL)
			}
		})
	}
}

// newTestDoHResolver points a resolver at the fake DoH server with a plain
// query client: the production client dials through the SSRF-safe dialer,
// which (correctly) refuses the test server's loopback address.
func newTestDoHResolver(t *testing.T, handler http.HandlerFunc) (*dohResolver, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	r := newDoHResolver(srv.URL)
	r.client = &http.Client{Timeout: 5 * time.Second}
	return r, &hits
}

func TestDoHResolverQueryAndCache(t *testing.T) {
	resolver, hits := newTestDoHResolver(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("name"); got != "example.com" {
			t.Errorf("query name = %q, want example.com", got)
		}
		if got := r.URL.Query().Get("type"); got != "A" {
			t.Errorf("query type = %q, want A", got)
		}
		if got := r.Header.Get("Accept"); got != "application/dns-json" {
			t.Errorf("Accept = %q, want application/dns-json", got)
		}
		w.Header().Set("Content-Type", "application/dns-json")
		fmt.Fprint(w, `{"Status":0,"Answer":[{"name":"example.com","type":1,"TTL":300,"data":"203.0.113.10"}]}`)
	})

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		ips, err := resolver.resolveA(ctx, "example.com")
		if err != nil {
			t.Fatalf("resolveA() call %d: %v", i, err)
		}
		if len(ips) != 1 || ips[0] != "203.0.113.10" {
			t.Fatalf("resolveA() = %v, want [203.0.113.10]", ips)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("DoH server hits = %d, want 1 (second resolve must come from cache)", got)
	}

	target, err := resolver.resolveDialTarget(ctx, "example.com", "example.com:443")
	if err != nil {
		t.Fatalf("resolveDialTarget() error: %v", err)
	}
	if target != "203.0.113.10:443" {
		t.Errorf("resolveDialTarget() = %q, want 203.0.113.10:443", target)
	}

	literal, err := resolver.resolveDialTarget(ctx, "203.0.113.99", "203.0.113.99:443")
	if err != nil {
		t.Fatalf("resolveDialTarget() literal IP error: %v", err)
	}
	if literal != "203.0.113.99:443" {
		t.Errorf("resolveDialTarget() literal = %q, want passthrough", literal)
	}
}

// TestDoHResolverFailover proves a failing endpoint does not fail the
// resolve when a later endpoint answers: the production failure this guards
// was a stalled one.one.one.one turning strict-pinned requests into 502s
// while cloudflare and google were healthy.
func TestDoHResolverFailover(t *testing.T) {
	var badHits, goodHits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		w.Header().Set("Content-Type", "application/dns-json")
		fmt.Fprint(w, `{"Status":0,"Answer":[{"name":"example.com","type":1,"TTL":300,"data":"203.0.113.20"}]}`)
	}))
	defer good.Close()

	r := newDoHResolver(bad.URL, good.URL)
	r.client = &http.Client{Timeout: 5 * time.Second}
	ips, err := r.resolveA(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("resolveA() with failover: %v", err)
	}
	if len(ips) != 1 || ips[0] != "203.0.113.20" {
		t.Fatalf("resolveA() = %v, want [203.0.113.20] from the healthy endpoint", ips)
	}
	if badHits.Load() == 0 {
		t.Errorf("failing endpoint was never tried")
	}
	if goodHits.Load() == 0 {
		t.Errorf("healthy endpoint was never tried")
	}

	// Every endpoint failing surfaces an error rather than a bogus answer.
	dead := newDoHResolver(bad.URL, "http://127.0.0.1:1/dns-query")
	dead.client = &http.Client{Timeout: 5 * time.Second}
	if _, err := dead.resolveA(context.Background(), "example.com"); err == nil {
		t.Errorf("resolveA() with all endpoints down = nil error, want failure")
	}
}

func TestDoHResolverRejectsPrivateAnswer(t *testing.T) {
	resolver, _ := newTestDoHResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-json")
		fmt.Fprint(w, `{"Status":0,"Answer":[{"name":"example.com","type":1,"TTL":300,"data":"192.168.0.53"}]}`)
	})
	if _, err := resolver.resolveA(context.Background(), "example.com"); err == nil {
		t.Fatal("resolveA() with a private-IP answer must fail, got nil error")
	}
}

func TestNewSafeTransportWithDoH(t *testing.T) {
	tr := NewSafeTransportWithDoH("cloudflare")
	if tr.DialContext == nil {
		t.Error("DialContext is nil, want the DoH plain dial")
	}
	if tr.DialTLSContext == nil {
		t.Error("DialTLSContext is nil, want the uTLS DoH dial")
	}
	if got := lookupTransportProxy(tr).kind; got != proxyKindEnv {
		t.Errorf("transport proxy kind = %v, want proxyKindEnv (ambient proxy tunneling)", got)
	}
}

// A TLS failure on a DoH path must be able to name the address and resolver
// the request was dialed to; without that the log line carries only a
// hostname and the operator cannot tell a sinkhole from a re-signing proxy.
func TestDoHResolutionNoteNamesAddressAndResolver(t *testing.T) {
	const host = "note.example"
	rememberDoHResolution(host, "203.0.113.9", "https://dns.google/resolve")

	ip, resolver := resolutionNote("https://" + host + "/v2/chat/completions")
	if ip != "203.0.113.9" {
		t.Fatalf("resolutionNote() ip = %q, want 203.0.113.9", ip)
	}
	if !strings.Contains(resolver, "dns.google") {
		t.Fatalf("resolutionNote() resolver = %q, want the answering endpoint", resolver)
	}

	summary := resolutionSummary(ip, resolver)
	for _, want := range []string{"203.0.113.9", "dns.google"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("resolutionSummary() = %q, want it to mention %q", summary, want)
		}
	}

	if ip, resolver := resolutionNote("https://never-resolved.example/"); ip != "" || resolver != "" {
		t.Fatalf("resolutionNote() for an unresolved host = %q/%q, want empty", ip, resolver)
	}
	if got := resolutionSummary("", ""); got != "no DoH resolution recorded" {
		t.Fatalf("resolutionSummary() with no note = %q", got)
	}
}
