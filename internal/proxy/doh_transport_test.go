package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	targets, err := resolver.resolveDialTargets(ctx, "example.com", "example.com:443")
	if err != nil {
		t.Fatalf("resolveDialTargets() error: %v", err)
	}
	if len(targets) != 1 || targets[0] != "203.0.113.10:443" {
		t.Errorf("resolveDialTargets() = %v, want [203.0.113.10:443]", targets)
	}

	literal, err := resolver.resolveDialTargets(ctx, "203.0.113.99", "203.0.113.99:443")
	if err != nil {
		t.Fatalf("resolveDialTargets() literal IP error: %v", err)
	}
	if len(literal) != 1 || literal[0] != "203.0.113.99:443" {
		t.Errorf("resolveDialTargets() literal = %v, want the single passthrough", literal)
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

// TestDoHResolveDialTargetsReturnsEveryRecord pins the multi-address contract
// the dial path depends on.
//
// A DoH answer is a set, and a multi-homed provider routinely publishes
// addresses this network cannot reach (observed live: 5 of 8 kiro runtime
// records and 1 of 2 kilo records answered the proxy's CONNECT with 502).
// Returning a single pick turned every request into a coin flip; returning the
// whole answer is what lets dialTunnel walk past a dead record.
func TestDoHResolveDialTargetsReturnsEveryRecord(t *testing.T) {
	resolver, _ := newTestDoHResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-json")
		fmt.Fprint(w, `{"Status":0,"Answer":[
			{"name":"multi.example","type":1,"TTL":300,"data":"203.0.113.1"},
			{"name":"multi.example","type":1,"TTL":300,"data":"203.0.113.2"},
			{"name":"multi.example","type":1,"TTL":300,"data":"203.0.113.3"}]}`)
	})

	targets, err := resolver.resolveDialTargets(context.Background(), "multi.example", "multi.example:443")
	if err != nil {
		t.Fatalf("resolveDialTargets() error: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("resolveDialTargets() = %v, want all 3 records", targets)
	}
	for _, want := range []string{"203.0.113.1:443", "203.0.113.2:443", "203.0.113.3:443"} {
		found := false
		for _, got := range targets {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("resolveDialTargets() = %v, missing %q", targets, want)
		}
	}

	// Rotation still moves: successive calls must not always start at the same
	// record, or a dead first entry would be retried by every request.
	first := targets[0]
	rotated := false
	for i := 0; i < 3; i++ {
		next, err := resolver.resolveDialTargets(context.Background(), "multi.example", "multi.example:443")
		if err != nil {
			t.Fatalf("resolveDialTargets() call %d: %v", i, err)
		}
		if next[0] != first {
			rotated = true
		}
	}
	if !rotated {
		t.Errorf("resolveDialTargets() always starts at %q, want the cursor to rotate", first)
	}

	// A literal IP still passes through untouched, exactly once.
	literal, err := resolver.resolveDialTargets(context.Background(), "203.0.113.99", "203.0.113.99:443")
	if err != nil {
		t.Fatalf("resolveDialTargets() literal IP error: %v", err)
	}
	if len(literal) != 1 || literal[0] != "203.0.113.99:443" {
		t.Errorf("resolveDialTargets() literal = %v, want the single passthrough", literal)
	}
}

// TestDoHResolveACollapsesConcurrentMisses pins the singleflight around the
// DoH query.
//
// A burst of requests to one provider (a retry storm, a dashboard's parallel
// probes) used to fire one DoH query per request. The resolvers answered the
// pile-up by timing out every attempt: observed live as six concurrent requests
// to one host failing with "context deadline exceeded" on all four resolvers
// while the same host answered in ~1.2s on its own.
func TestDoHResolveACollapsesConcurrentMisses(t *testing.T) {
	var hits atomic.Int64
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	resolver, _ := newTestDoHResolver(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		// Hold the first query open so the other callers are guaranteed to
		// arrive while it is still in flight.
		<-release
		w.Header().Set("Content-Type", "application/dns-json")
		fmt.Fprint(w, `{"Status":0,"Answer":[{"name":"burst.example","type":1,"TTL":300,"data":"203.0.113.7"}]}`)
	})

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = resolver.resolveA(context.Background(), "burst.example")
		}(i)
	}

	<-entered
	// Give the followers time to join the flight before releasing the leader.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: resolveA() error = %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("DoH queries = %d, want 1 (concurrent misses must collapse)", got)
	}
}

// TestDoHResolveAFlightSurvivesLeaderCancellation pins that a shared lookup is
// not torn down when the caller who started it disconnects — the failure the
// live catalog flight already had to fix. Without WithoutCancel here, one
// cancelled client fails every waiter queued behind it.
func TestDoHResolveAFlightSurvivesLeaderCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	resolver, _ := newTestDoHResolver(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/dns-json")
		fmt.Fprint(w, `{"Status":0,"Answer":[{"name":"cancel.example","type":1,"TTL":300,"data":"203.0.113.8"}]}`)
	})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := resolver.resolveA(leaderCtx, "cancel.example")
		leaderDone <- err
	}()

	<-entered
	cancelLeader()
	time.Sleep(50 * time.Millisecond)

	followerDone := make(chan error, 1)
	go func() {
		_, err := resolver.resolveA(context.Background(), "cancel.example")
		followerDone <- err
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)

	<-leaderDone
	if err := <-followerDone; err != nil {
		t.Fatalf("follower resolveA() error = %v, want the shared query to survive the leader's cancellation", err)
	}
}

// TestDoHDialFailsOverAcrossRecords pins the dial-side half of the fix: when a
// DoH answer carries several records and the first cannot be tunneled to, the
// dial must try the next one instead of failing the request.
//
// This is the exact shape that broke kiro, kilo and cline through a DoH pool:
// a multi-homed provider publishes records the network cannot reach, and
// dialing only the first pick failed every request that drew a dead record.
func TestDoHDialFailsOverAcrossRecords(t *testing.T) {
	// The fake proxy refuses one address and accepts the other, which is what
	// the ambient egress proxy does with an unreachable CDN record.
	var refused, accepted atomic.Int64
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if strings.Contains(r.Host, "203.0.113.1:") {
			refused.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		accepted.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		// Hold the tunnel open just long enough for the dialer to return it.
		time.Sleep(50 * time.Millisecond)
	}))
	defer proxySrv.Close()

	// Both records look public; only the second is accepted by the fake proxy.
	resolver, _ := newTestDoHResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-json")
		fmt.Fprint(w, `{"Status":0,"Answer":[
			{"name":"failover.example","type":1,"TTL":300,"data":"203.0.113.1"},
			{"name":"failover.example","type":1,"TTL":300,"data":"203.0.113.2"}]}`)
	})

	// The test proxy is on loopback; the SSRF guard would otherwise refuse to
	// dial it. This is the same exemption an operator's local proxy gets.
	t.Setenv("ALLOW_PRIVATE_NETWORKING", "1")

	d := &utlsDialer{explicitProxyURL: proxySrv.URL, doh: resolver}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, err := d.dialPlainContext(ctx, "tcp", "failover.example:443")
	if err != nil {
		t.Fatalf("dialPlainContext() = %v, want it to fail over past the refused record", err)
	}
	conn.Close()

	if refused.Load() == 0 {
		t.Error("the unreachable record was never attempted; the test proves nothing")
	}
	if accepted.Load() == 0 {
		t.Error("the reachable record was never attempted")
	}
}

// TestDoHDialRecordsTheAddressThatAccepted pins that the diagnostic note names
// the record the request actually went to.
//
// Once the dial fails over between records, the address dialed is no longer the
// address the resolver was asked about. A later TLS failure that reported the
// first candidate would send the operator to the wrong host.
func TestDoHDialRecordsTheAddressThatAccepted(t *testing.T) {
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Host, "203.0.113.1:") {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		time.Sleep(50 * time.Millisecond)
	}))
	defer proxySrv.Close()

	resolver, _ := newTestDoHResolver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-json")
		fmt.Fprint(w, `{"Status":0,"Answer":[
			{"name":"note-dial.example","type":1,"TTL":300,"data":"203.0.113.1"},
			{"name":"note-dial.example","type":1,"TTL":300,"data":"203.0.113.2"}]}`)
	})
	t.Setenv("ALLOW_PRIVATE_NETWORKING", "1")

	d := &utlsDialer{explicitProxyURL: proxySrv.URL, doh: resolver}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := d.dialPlainContext(ctx, "tcp", "note-dial.example:443")
	if err != nil {
		t.Fatalf("dialPlainContext() = %v", err)
	}
	conn.Close()

	ip, _, ok := DoHResolutionFor("note-dial.example")
	if !ok {
		t.Fatal("no DoH resolution recorded, want the address that accepted")
	}
	// The cursor starts at the first record, which the fake proxy refuses, so
	// the dial must have failed over to the second and recorded that one.
	if ip != "203.0.113.2" {
		t.Errorf("recorded %q, want 203.0.113.2 (the record that accepted)", ip)
	}
}
