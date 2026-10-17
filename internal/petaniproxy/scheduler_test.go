package petaniproxy

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeSchedulerPool is a mutex-guarded in-memory SchedulerPool for tests.
type fakeSchedulerPool struct {
	mu      sync.Mutex
	proxies []PoolProxy
}

func newFakeSchedulerPool(proxies ...PoolProxy) *fakeSchedulerPool {
	return &fakeSchedulerPool{proxies: append([]PoolProxy(nil), proxies...)}
}

func (f *fakeSchedulerPool) All() []PoolProxy {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PoolProxy(nil), f.proxies...)
}

func (f *fakeSchedulerPool) Replace(proxies []PoolProxy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proxies = append([]PoolProxy(nil), proxies...)
}

func (f *fakeSchedulerPool) Add(p PoolProxy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proxies = append(f.proxies, p)
}

func (f *fakeSchedulerPool) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.proxies)
}

func TestSchedulerPoolProxyKey(t *testing.T) {
	tests := []struct {
		name  string
		proxy PoolProxy
		want  string
	}{
		{"explicit proxy field wins", PoolProxy{FetcherProxyCandidate: FetcherProxyCandidate{Proxy: "9.9.9.9:3128", IP: "1.1.1.1", Port: 8080}}, "9.9.9.9:3128"},
		{"falls back to ip:port", PoolProxy{FetcherProxyCandidate: FetcherProxyCandidate{IP: "1.2.3.4", Port: 1080}}, "1.2.3.4:1080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.proxy.Key(); got != tt.want {
				t.Errorf("Key() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSchedulerNewDefaults(t *testing.T) {
	pool := newFakeSchedulerPool()
	c := NewPoolHealthChecker(pool, HealthCheckerConfig{})
	if c.checkInterval != 90*time.Second {
		t.Errorf("checkInterval = %v, want 90s", c.checkInterval)
	}
	if c.minHealthyCount != 5 {
		t.Errorf("minHealthyCount = %d, want 5", c.minHealthyCount)
	}
	if c.maxProbeTimeout != 3*time.Second {
		t.Errorf("maxProbeTimeout = %v, want 3s", c.maxProbeTimeout)
	}
	if !c.enableAutoRefill {
		t.Error("enableAutoRefill = false, want true (Python default)")
	}
	if c.refillCooldown != DefaultRefillCooldown {
		t.Errorf("refillCooldown = %v, want %v", c.refillCooldown, DefaultRefillCooldown)
	}
	if c.probe == nil {
		t.Error("probe = nil, want DefaultProbeSingleProxy")
	}
}

func TestSchedulerCheckAndPrune(t *testing.T) {
	alive := map[string]bool{
		"1.1.1.1:8080": true,
		"2.2.2.2:8080": false,
		"3.3.3.3:8080": true,
		"4.4.4.4:8080": false,
	}
	probe := func(_ context.Context, p PoolProxy, _ time.Duration) bool {
		return alive[p.Key()]
	}
	mk := func(keys ...string) []PoolProxy {
		var out []PoolProxy
		for _, k := range keys {
			out = append(out, PoolProxy{FetcherProxyCandidate: FetcherProxyCandidate{Proxy: k, Protocol: "http"}})
		}
		return out
	}
	tests := []struct {
		name         string
		initial      []PoolProxy
		wantHealthy  int
		wantEvicted  int
		wantPoolKeys []string
		wantProbes   int
	}{
		{
			name:         "all healthy keeps pool",
			initial:      mk("1.1.1.1:8080", "3.3.3.3:8080"),
			wantHealthy:  2,
			wantEvicted:  0,
			wantPoolKeys: []string{"1.1.1.1:8080", "3.3.3.3:8080"},
			wantProbes:   2,
		},
		{
			name:         "dead proxies are pruned",
			initial:      mk("1.1.1.1:8080", "2.2.2.2:8080", "3.3.3.3:8080", "4.4.4.4:8080"),
			wantHealthy:  2,
			wantEvicted:  2,
			wantPoolKeys: []string{"1.1.1.1:8080", "3.3.3.3:8080"},
			wantProbes:   4,
		},
		{
			name:         "all dead empties pool",
			initial:      mk("2.2.2.2:8080", "4.4.4.4:8080"),
			wantHealthy:  0,
			wantEvicted:  2,
			wantPoolKeys: nil,
			wantProbes:   2,
		},
		{
			name:         "empty pool probes nothing",
			initial:      nil,
			wantHealthy:  0,
			wantEvicted:  0,
			wantPoolKeys: nil,
			wantProbes:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := newFakeSchedulerPool(tt.initial...)
			probeCalls := 0
			counting := func(ctx context.Context, p PoolProxy, d time.Duration) bool {
				probeCalls++
				return probe(ctx, p, d)
			}
			c := NewPoolHealthChecker(pool, HealthCheckerConfig{Probe: counting, EnableAutoRefill: false})
			healthy, evicted := c.CheckAndPrune(context.Background())
			if healthy != tt.wantHealthy {
				t.Errorf("healthy = %d, want %d", healthy, tt.wantHealthy)
			}
			if evicted != tt.wantEvicted {
				t.Errorf("evicted = %d, want %d", evicted, tt.wantEvicted)
			}
			if probeCalls != tt.wantProbes {
				t.Errorf("probe calls = %d, want %d", probeCalls, tt.wantProbes)
			}
			var gotKeys []string
			for _, p := range pool.All() {
				gotKeys = append(gotKeys, p.Key())
			}
			if len(gotKeys) != len(tt.wantPoolKeys) {
				t.Fatalf("pool keys = %v, want %v", gotKeys, tt.wantPoolKeys)
			}
			for i := range gotKeys {
				if gotKeys[i] != tt.wantPoolKeys[i] {
					t.Errorf("pool keys = %v, want %v", gotKeys, tt.wantPoolKeys)
					break
				}
			}
			if got := c.Stats().TotalEvicted; got != int64(tt.wantEvicted) {
				t.Errorf("TotalEvicted = %d, want %d", got, tt.wantEvicted)
			}
		})
	}
}

func TestSchedulerTriggerRefillDedup(t *testing.T) {
	pool := newFakeSchedulerPool(
		PoolProxy{FetcherProxyCandidate: FetcherProxyCandidate{Proxy: "1.1.1.1:8080", Protocol: "http"}},
	)
	newbie := PoolProxy{FetcherProxyCandidate: FetcherProxyCandidate{Proxy: "2.2.2.2:8080", Protocol: "socks5"}}
	dup := PoolProxy{FetcherProxyCandidate: FetcherProxyCandidate{Proxy: "1.1.1.1:8080", Protocol: "http"}}
	c := NewPoolHealthChecker(pool, HealthCheckerConfig{
		EnableAutoRefill: false,
		Refill: func(_ context.Context) []PoolProxy {
			return []PoolProxy{dup, newbie, newbie} // dup + intra-batch dup
		},
	})
	added := c.TriggerRefill(context.Background())
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	if pool.Len() != 2 {
		t.Errorf("pool len = %d, want 2", pool.Len())
	}
	if got := c.Stats().TotalRefilled; got != 1 {
		t.Errorf("TotalRefilled = %d, want 1", got)
	}
	if c.Stats().LastRefillTime.IsZero() {
		t.Error("LastRefillTime not set after successful refill")
	}
}

func TestSchedulerTriggerRefillEdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		refill     RefillFunc
		wantAdded  int
		wantRefill bool // whether LastRefillTime should be set
	}{
		{"nil refill func", nil, 0, false},
		{"empty result", func(_ context.Context) []PoolProxy { return nil }, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := newFakeSchedulerPool()
			c := NewPoolHealthChecker(pool, HealthCheckerConfig{Refill: tt.refill, EnableAutoRefill: false})
			if got := c.TriggerRefill(context.Background()); got != tt.wantAdded {
				t.Errorf("added = %d, want %d", got, tt.wantAdded)
			}
			if got := c.Stats().TotalRefilled; got != 0 {
				t.Errorf("TotalRefilled = %d, want 0", got)
			}
			if isSet := !c.Stats().LastRefillTime.IsZero(); isSet != tt.wantRefill {
				t.Errorf("LastRefillTime set = %v, want %v", isSet, tt.wantRefill)
			}
		})
	}
}

func TestSchedulerStartStopTicker(t *testing.T) {
	pool := newFakeSchedulerPool(
		PoolProxy{FetcherProxyCandidate: FetcherProxyCandidate{Proxy: "1.1.1.1:8080", Protocol: "http"}},
	)
	var probeCalls, refillCalls int64
	var mu sync.Mutex
	bump := func(p *int64) { mu.Lock(); *p++; mu.Unlock() }
	read := func(p *int64) int64 { mu.Lock(); defer mu.Unlock(); return *p }

	c := NewPoolHealthChecker(pool, HealthCheckerConfig{
		CheckInterval:    25 * time.Millisecond,
		MinHealthyCount:  100, // always below threshold -> refill path active
		RefillCooldown:   40 * time.Millisecond,
		EnableAutoRefill: true,
		Probe: func(_ context.Context, _ PoolProxy, _ time.Duration) bool {
			bump(&probeCalls)
			return true
		},
		Refill: func(_ context.Context) []PoolProxy {
			bump(&refillCalls)
			return []PoolProxy{{FetcherProxyCandidate: FetcherProxyCandidate{
				Proxy:    "9.9.9.9:9999",
				Protocol: "http",
			}}}
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	if !c.Running() {
		t.Fatal("Running() = false after Start")
	}
	c.Start(ctx) // idempotent: second Start must not spawn another loop
	time.Sleep(300 * time.Millisecond)
	c.Stop()
	if c.Running() {
		t.Fatal("Running() = true after Stop")
	}
	c.Stop() // idempotent

	if got := read(&probeCalls); got < 2 {
		t.Errorf("probe calls = %d, want >= 2 (ticker never fired?)", got)
	}
	if got := read(&refillCalls); got < 1 {
		t.Errorf("refill calls = %d, want >= 1 (auto-refill never fired?)", got)
	}
	if got := read(&refillCalls); got > 8 {
		t.Errorf("refill calls = %d, want <= 8 (cooldown not respected?)", got)
	}
	if c.Stats().LastCheckTime.IsZero() {
		t.Error("LastCheckTime not set after ticker ran")
	}

	// Restart works after Stop.
	c.Start(ctx)
	time.Sleep(60 * time.Millisecond)
	c.Stop()
	if got := read(&probeCalls); got < 2 {
		t.Errorf("probe calls after restart = %d, want growth", got)
	}
}

func TestSchedulerStopCancelsOnContext(t *testing.T) {
	pool := newFakeSchedulerPool()
	c := NewPoolHealthChecker(pool, HealthCheckerConfig{
		CheckInterval:    10 * time.Millisecond,
		EnableAutoRefill: false,
		Probe:            func(_ context.Context, _ PoolProxy, _ time.Duration) bool { return true },
	})
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	cancel() // loop must exit via ctx.Done()
	time.Sleep(100 * time.Millisecond)
	c.Stop() // must not hang or panic
}

func TestSchedulerDefaultProbeRejectsInvalid(t *testing.T) {
	tests := []struct {
		name  string
		proxy PoolProxy
	}{
		{"empty proxy", PoolProxy{}},
		{"unparseable target", PoolProxy{FetcherProxyCandidate: FetcherProxyCandidate{Proxy: ":::not a url:::", Protocol: "http"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a tiny timeout and a cancelled context guard: invalid inputs
			// must fail fast without touching the network.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if DefaultProbeSingleProxy(ctx, tt.proxy, time.Second) {
				t.Errorf("DefaultProbeSingleProxy(%q) = true, want false", tt.proxy.Key())
			}
		})
	}
}
