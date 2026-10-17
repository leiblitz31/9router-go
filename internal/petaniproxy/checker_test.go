package petaniproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckerClassifyAnonymity(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		egressIP string
		hostIP   string
		expected string
	}{
		{
			name:     "egress equals host is transparent",
			headers:  map[string]string{},
			egressIP: "1.2.3.4",
			hostIP:   "1.2.3.4",
			expected: AnonymityTransparent,
		},
		{
			name:     "header leaks host ip is transparent",
			headers:  map[string]string{"X-Some-Header": "client=9.9.9.9 seen"},
			egressIP: "5.6.7.8",
			hostIP:   "9.9.9.9",
			expected: AnonymityTransparent,
		},
		{
			name:     "via header is anonymous",
			headers:  map[string]string{"Via": "1.1 test-proxy"},
			egressIP: "5.6.7.8",
			hostIP:   "9.9.9.9",
			expected: AnonymityAnonymous,
		},
		{
			name:     "x-forwarded-for header is anonymous",
			headers:  map[string]string{"X-Forwarded-For": "5.6.7.8"},
			egressIP: "5.6.7.8",
			hostIP:   "9.9.9.9",
			expected: AnonymityAnonymous,
		},
		{
			name:     "no indicators is elite",
			headers:  map[string]string{"Content-Type": "application/json"},
			egressIP: "5.6.7.8",
			hostIP:   "9.9.9.9",
			expected: AnonymityElite,
		},
		{
			name:     "no host ip known and no indicators is elite",
			headers:  map[string]string{},
			egressIP: "5.6.7.8",
			hostIP:   "",
			expected: AnonymityElite,
		},
		{
			name:     "empty egress with host set is elite",
			headers:  map[string]string{},
			egressIP: "",
			hostIP:   "9.9.9.9",
			expected: AnonymityElite,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			if got := ClassifyAnonymity(h, tt.egressIP, tt.hostIP); got != tt.expected {
				t.Errorf("ClassifyAnonymity() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestCheckerGetHostIP(t *testing.T) {
	t.Run("json endpoint", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			fmt.Fprint(w, `{"ip":"1.2.3.4"}`)
		}))
		defer srv.Close()
		overrideHostIPEndpoints(t, srv.URL)
		if got := GetHostIP(); got != "1.2.3.4" {
			t.Fatalf("GetHostIP() = %q, want %q", got, "1.2.3.4")
		}
		// Second call must use the cache, not the network.
		if got := GetHostIP(); got != "1.2.3.4" {
			t.Fatalf("GetHostIP() cached = %q, want %q", got, "1.2.3.4")
		}
		if n := atomic.LoadInt32(&hits); n != 1 {
			t.Errorf("endpoint hit %d times, want 1 (cache not used)", n)
		}
	})

	t.Run("plain text endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "9.9.9.9\n")
		}))
		defer srv.Close()
		overrideHostIPEndpoints(t, srv.URL)
		if got := GetHostIP(); got != "9.9.9.9" {
			t.Errorf("GetHostIP() = %q, want %q", got, "9.9.9.9")
		}
	})

	t.Run("falls through to next endpoint", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer bad.Close()
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"ip":"7.7.7.7"}`)
		}))
		defer good.Close()
		// First endpoint returns 200 with an empty body -> treated as failure.
		overrideHostIPEndpoints(t, bad.URL, good.URL)
		if got := GetHostIP(); got != "7.7.7.7" {
			t.Errorf("GetHostIP() = %q, want %q", got, "7.7.7.7")
		}
	})

	t.Run("all endpoints fail returns empty", func(t *testing.T) {
		overrideHostIPEndpoints(t, "http://127.0.0.1:1/", "http://127.0.0.1:2/")
		if got := GetHostIP(); got != "" {
			t.Errorf("GetHostIP() = %q, want empty", got)
		}
	})
}

func TestCheckerTestSingleProxy(t *testing.T) {
	proxyAddr := startForwardProxy(t)

	t.Run("http proxy elite", func(t *testing.T) {
		target := startIPServer(t, "5.6.7.8", nil)
		lp, err := TestSingleProxy(context.Background(), testCandidate(proxyAddr, "http"), 5*time.Second, target, "9.9.9.9")
		if err != nil {
			t.Fatalf("TestSingleProxy() error = %v", err)
		}
		if lp.EgressIP != "5.6.7.8" {
			t.Errorf("EgressIP = %q, want %q", lp.EgressIP, "5.6.7.8")
		}
		if lp.Anonymity != AnonymityElite {
			t.Errorf("Anonymity = %q, want %q", lp.Anonymity, AnonymityElite)
		}
		if lp.Protocol != "http" || lp.Proxy != proxyAddr {
			t.Errorf("Protocol/Proxy = %q/%q", lp.Protocol, lp.Proxy)
		}
		if lp.LatencyMs < 0 {
			t.Errorf("LatencyMs = %d, want >= 0", lp.LatencyMs)
		}
		if lp.Country != "Unknown" || lp.CountryCode != "??" || lp.City != "-" || lp.ISP != "-" {
			t.Errorf("geo defaults not applied: %+v", lp)
		}
	})

	t.Run("transparent when egress matches host", func(t *testing.T) {
		target := startIPServer(t, "9.9.9.9", nil)
		lp, err := TestSingleProxy(context.Background(), testCandidate(proxyAddr, "http"), 5*time.Second, target, "9.9.9.9")
		if err != nil {
			t.Fatalf("TestSingleProxy() error = %v", err)
		}
		if lp.Anonymity != AnonymityTransparent {
			t.Errorf("Anonymity = %q, want %q", lp.Anonymity, AnonymityTransparent)
		}
	})

	t.Run("anonymous when target sends via header", func(t *testing.T) {
		target := startIPServer(t, "5.6.7.8", map[string]string{"Via": "1.1 test-proxy"})
		lp, err := TestSingleProxy(context.Background(), testCandidate(proxyAddr, "http"), 5*time.Second, target, "9.9.9.9")
		if err != nil {
			t.Fatalf("TestSingleProxy() error = %v", err)
		}
		if lp.Anonymity != AnonymityAnonymous {
			t.Errorf("Anonymity = %q, want %q", lp.Anonymity, AnonymityAnonymous)
		}
	})

	t.Run("non-2xx status is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "denied", http.StatusForbidden)
		}))
		defer srv.Close()
		if _, err := TestSingleProxy(context.Background(), testCandidate(proxyAddr, "http"), 5*time.Second, srv.URL, "9.9.9.9"); err == nil {
			t.Error("TestSingleProxy() expected error for 403, got nil")
		}
	})

	t.Run("dead proxy is an error", func(t *testing.T) {
		target := startIPServer(t, "5.6.7.8", nil)
		if _, err := TestSingleProxy(context.Background(), testCandidate("127.0.0.1:1", "http"), 2*time.Second, target, "9.9.9.9"); err == nil {
			t.Error("TestSingleProxy() expected error for dead proxy, got nil")
		}
	})

	t.Run("socks5 proxy", func(t *testing.T) {
		socksAddr := startSocks5Server(t)
		target := startIPServer(t, "5.6.7.8", nil)
		lp, err := TestSingleProxy(context.Background(), testCandidate(socksAddr, "socks5"), 5*time.Second, target, "9.9.9.9")
		if err != nil {
			t.Fatalf("TestSingleProxy() socks5 error = %v", err)
		}
		if lp.EgressIP != "5.6.7.8" || lp.Protocol != "socks5" {
			t.Errorf("unexpected result: %+v", lp)
		}
	})

	t.Run("socks4 proxy", func(t *testing.T) {
		socksAddr := startSocks4Server(t)
		target := startIPServer(t, "5.6.7.8", nil)
		lp, err := TestSingleProxy(context.Background(), testCandidate(socksAddr, "socks4"), 5*time.Second, target, "9.9.9.9")
		if err != nil {
			t.Fatalf("TestSingleProxy() socks4 error = %v", err)
		}
		if lp.EgressIP != "5.6.7.8" || lp.Protocol != "socks4" {
			t.Errorf("unexpected result: %+v", lp)
		}
	})

	t.Run("unsupported protocol is an error", func(t *testing.T) {
		target := startIPServer(t, "5.6.7.8", nil)
		if _, err := TestSingleProxy(context.Background(), testCandidate(proxyAddr, "gopher"), 2*time.Second, target, ""); err == nil {
			t.Error("TestSingleProxy() expected error for bad protocol, got nil")
		}
	})

	t.Run("context cancel aborts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		target := startIPServer(t, "5.6.7.8", nil)
		if _, err := TestSingleProxy(ctx, testCandidate(proxyAddr, "http"), 5*time.Second, target, ""); err == nil {
			t.Error("TestSingleProxy() expected error for cancelled context, got nil")
		}
	})
}

func TestCheckerBatchEnrichGeoIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
			{"query":"1.1.1.1","status":"success","country":"United States","countryCode":"US","city":"Austin","isp":"Cloudflare"},
			{"query":"2.2.2.2","status":"fail","message":"reserved range"}
		]`)
	}))
	defer srv.Close()
	overrideGeoIPBatchURL(t, srv.URL)

	live := []*LiveProxy{
		{IP: "1.1.1.1", Country: "Unknown", CountryCode: "??", City: "-", ISP: "-"},
		{IP: "2.2.2.2", Country: "Unknown", CountryCode: "??", City: "-", ISP: "-"},
	}
	BatchEnrichGeoIP(context.Background(), live)

	if live[0].Country != "United States" || live[0].CountryCode != "US" ||
		live[0].City != "Austin" || live[0].ISP != "Cloudflare" {
		t.Errorf("proxy 1 not enriched: %+v", live[0])
	}
	if live[1].Country != "Unknown" || live[1].CountryCode != "??" {
		t.Errorf("failed lookup should keep defaults: %+v", live[1])
	}

	// Empty input and unreachable endpoint must not panic.
	BatchEnrichGeoIP(context.Background(), nil)
	overrideGeoIPBatchURL(t, "http://127.0.0.1:1/batch")
	BatchEnrichGeoIP(context.Background(), live)
}

func TestCheckerCheckProxiesPool(t *testing.T) {
	proxyAddr := startForwardProxy(t)
	target := startIPServer(t, "5.6.7.8", nil)

	newOpts := func() CheckPoolOptions {
		return CheckPoolOptions{
			MaxCheck:    10,
			TargetAlive: 2,
			Timeout:     5 * time.Second,
			MaxWorkers:  4,
			TestURL:     target,
			HostIP:      "9.9.9.9", // avoid real host-IP lookup
		}
	}
	candidates := []ProxyCandidate{
		testCandidate(proxyAddr, "http"),
		testCandidate(proxyAddr, "http"),
		testCandidate(proxyAddr, "http"),
		testCandidate("127.0.0.1:1", "http"), // dead
	}

	t.Run("early stop at target alive", func(t *testing.T) {
		overrideGeoIPBatchURL(t, "http://127.0.0.1:1/batch") // enrich fails silently
		var calls int32
		opts := newOpts()
		opts.OnLive = func(lp *LiveProxy, aliveCount, target int) {
			atomic.AddInt32(&calls, 1)
			if aliveCount > target {
				t.Errorf("OnLive aliveCount %d exceeds target %d", aliveCount, target)
			}
		}
		got := CheckProxiesPool(context.Background(), candidates, opts)
		if len(got) != 2 {
			t.Fatalf("CheckProxiesPool() returned %d, want 2 (early stop)", len(got))
		}
		if n := atomic.LoadInt32(&calls); n != 2 {
			t.Errorf("OnLive called %d times, want 2", n)
		}
		for i := 1; i < len(got); i++ {
			if got[i].LatencyMs < got[i-1].LatencyMs {
				t.Errorf("not sorted by latency: %+v", got)
				break
			}
		}
	})

	t.Run("anonymity filter", func(t *testing.T) {
		overrideGeoIPBatchURL(t, "http://127.0.0.1:1/batch")
		opts := newOpts()
		opts.TargetAlive = 10
		opts.AnonymityFilter = "elite"
		if got := CheckProxiesPool(context.Background(), candidates, opts); len(got) != 3 {
			t.Errorf("elite filter: got %d, want 3", len(got))
		}
		opts.AnonymityFilter = "transparent"
		if got := CheckProxiesPool(context.Background(), candidates, opts); len(got) != 0 {
			t.Errorf("transparent filter: got %d, want 0", len(got))
		}
		opts.AnonymityFilter = "all"
		if got := CheckProxiesPool(context.Background(), candidates, opts); len(got) != 3 {
			t.Errorf("'all' filter: got %d, want 3", len(got))
		}
	})

	t.Run("country filter", func(t *testing.T) {
		geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"status":"success","country":"United States","countryCode":"US","city":"Austin","isp":"Cloudflare"}`)
		}))
		defer geo.Close()
		overrideGeoIPSingleURLFmt(t, geo.URL+"/%s")

		opts := newOpts()
		opts.TargetAlive = 10
		opts.CountryFilter = "US"
		got := CheckProxiesPool(context.Background(), candidates, opts)
		if len(got) != 3 {
			t.Fatalf("country filter US: got %d, want 3", len(got))
		}
		if got[0].CountryCode != "US" || got[0].City != "Austin" {
			t.Errorf("geo not applied: %+v", got[0])
		}

		opts.CountryFilter = "DE"
		if got := CheckProxiesPool(context.Background(), candidates, opts); len(got) != 0 {
			t.Errorf("country filter DE: got %d, want 0", len(got))
		}
	})

	t.Run("max check limits candidates", func(t *testing.T) {
		overrideGeoIPBatchURL(t, "http://127.0.0.1:1/batch")
		opts := newOpts()
		opts.MaxCheck = 1
		opts.TargetAlive = 10
		if got := CheckProxiesPool(context.Background(), candidates, opts); len(got) != 1 {
			t.Errorf("MaxCheck=1: got %d, want 1", len(got))
		}
	})

	t.Run("empty candidates", func(t *testing.T) {
		if got := CheckProxiesPool(context.Background(), nil, newOpts()); len(got) != 0 {
			t.Errorf("got %d, want 0", len(got))
		}
	})
}
