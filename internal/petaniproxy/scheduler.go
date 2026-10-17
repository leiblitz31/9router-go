// Package petaniproxy — pool scheduler.
//
// Go port of petani-proxy/core/pool_scheduler.py: a 24/7 background health
// checker that probes every proxy in the active pool, evicts dead/laggy
// nodes, and triggers an automatic background refill when healthy capacity
// drops below a threshold.
package petaniproxy

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProbeTargetURL is the lightweight endpoint used for proxy health probes,
// mirroring the Python original (Cloudflare / ipify trace endpoint).
const ProbeTargetURL = "https://cloudflare.com/cdn-cgi/trace"

// DefaultRefillCooldown is the minimum time between two auto-refills, so a
// shrinking pool does not spam the harvester every check cycle.
const DefaultRefillCooldown = 60 * time.Second

// PoolProxy is one entry of the active proxy pool. It extends the shared
// FetcherProxyCandidate (ip, port, proxy "ip:port", protocol) with the
// enrichment fields the scheduler and exporter need.
type PoolProxy struct {
	FetcherProxyCandidate
	Anonymity   string  `json:"anonymity,omitempty"`   // "Elite" | "Anonymous" | "Transparent"
	LatencyMs   float64 `json:"latency_ms,omitempty"`  // measured round-trip, milliseconds
	LatencySec  float64 `json:"latency_sec,omitempty"` // measured round-trip, seconds
	CountryCode string  `json:"country_code,omitempty"`
	Country     string  `json:"country,omitempty"`
	City        string  `json:"city,omitempty"`
	ISP         string  `json:"isp,omitempty"`
	EgressIP    string  `json:"egress_ip,omitempty"`
}

// Key returns the pool-unique identity of the proxy ("ip:port").
func (p PoolProxy) Key() string {
	if p.Proxy != "" {
		return p.Proxy
	}
	return p.IP + ":" + strconv.Itoa(p.Port)
}

// SchedulerPool is the minimal pool surface the health checker needs. The
// concrete pool manager (ported from pool_manager.py by a sibling worker)
// can satisfy it with an adapter if its method set differs.
type SchedulerPool interface {
	// All returns a snapshot of every proxy currently in the pool.
	All() []PoolProxy
	// Replace swaps the pool contents (used after pruning dead nodes).
	Replace(proxies []PoolProxy)
	// Add appends one proxy to the pool (used by the auto-refill path).
	Add(p PoolProxy)
	// Len returns the current pool size.
	Len() int
}

// ProbeFunc tests one proxy and reports whether it is alive.
type ProbeFunc func(ctx context.Context, p PoolProxy, timeout time.Duration) bool

// RefillFunc harvests fresh proxies to top the pool back up. It replaces the
// Python `from core.fast_validator import run_fast_harvester` import, which
// has no Go equivalent inside this package; callers wire the real harvester.
type RefillFunc func(ctx context.Context) []PoolProxy

// HealthCheckerConfig tunes a PoolHealthChecker. Zero values are replaced by
// the Python defaults.
type HealthCheckerConfig struct {
	// CheckInterval is how often the pool is probed. Default 90s.
	CheckInterval time.Duration
	// MinHealthyCount triggers auto-refill when the pool drops below it.
	// Default 5.
	MinHealthyCount int
	// MaxProbeTimeout bounds a single probe. Default 3s.
	MaxProbeTimeout time.Duration
	// EnableAutoRefill turns the background refill on/off. Default true.
	EnableAutoRefill bool
	// RefillCooldown is the minimum gap between refills. Default 60s.
	RefillCooldown time.Duration
	// Probe overrides the default HTTP probe (useful in tests).
	Probe ProbeFunc
	// Refill supplies fresh proxies for the auto-refill path. Nil disables
	// refills even when EnableAutoRefill is true.
	Refill RefillFunc
}

// HealthCheckerStats is a point-in-time snapshot of checker counters.
type HealthCheckerStats struct {
	TotalEvicted   int64
	TotalRefilled  int64
	LastCheckTime  time.Time
	LastRefillTime time.Time
}

// PoolHealthChecker is the Go equivalent of the Python PoolHealthChecker
// thread: a daemon goroutine driven by a time.Ticker that probes the pool,
// prunes dead proxies and auto-refills below the healthy threshold.
type PoolHealthChecker struct {
	pool SchedulerPool

	checkInterval    time.Duration
	minHealthyCount  int
	maxProbeTimeout  time.Duration
	enableAutoRefill bool
	refillCooldown   time.Duration
	probe            ProbeFunc
	refill           RefillFunc

	totalEvicted   atomic.Int64
	totalRefilled  atomic.Int64
	lastCheckUnix  atomic.Int64 // unix nanos; 0 = never
	lastRefillUnix atomic.Int64

	mu      sync.Mutex
	running bool
	done    chan struct{}
}

// NewPoolHealthChecker builds a checker over pool, applying Python defaults
// for any zero-valued config field.
func NewPoolHealthChecker(pool SchedulerPool, cfg HealthCheckerConfig) *PoolHealthChecker {
	c := &PoolHealthChecker{
		pool:             pool,
		checkInterval:    cfg.CheckInterval,
		minHealthyCount:  cfg.MinHealthyCount,
		maxProbeTimeout:  cfg.MaxProbeTimeout,
		enableAutoRefill: cfg.EnableAutoRefill,
		refillCooldown:   cfg.RefillCooldown,
		probe:            cfg.Probe,
		refill:           cfg.Refill,
	}
	if c.checkInterval <= 0 {
		c.checkInterval = 90 * time.Second
	}
	if c.minHealthyCount <= 0 {
		c.minHealthyCount = 5
	}
	if c.maxProbeTimeout <= 0 {
		c.maxProbeTimeout = 3 * time.Second
	}
	if c.refillCooldown <= 0 {
		c.refillCooldown = DefaultRefillCooldown
	}
	if zeroHealthCheckerConfig(cfg) {
		// A bare zero config still wants auto-refill on, matching Python's
		// enable_auto_refill=True default. (Compared field-by-field because
		// == on structs with func fields panics when the funcs are non-nil.)
		c.enableAutoRefill = true
	}
	if c.probe == nil {
		c.probe = DefaultProbeSingleProxy
	}
	return c
}

// zeroHealthCheckerConfig reports whether every config field is unset,
// without comparing func values against each other (which panics when both
// are non-nil).
func zeroHealthCheckerConfig(cfg HealthCheckerConfig) bool {
	return cfg.CheckInterval == 0 &&
		cfg.MinHealthyCount == 0 &&
		cfg.MaxProbeTimeout == 0 &&
		cfg.RefillCooldown == 0 &&
		cfg.Probe == nil &&
		cfg.Refill == nil
}

// Start launches the background ticker goroutine. It is a no-op if the
// checker is already running. The goroutine exits when Stop is called or ctx
// is cancelled.
func (c *PoolHealthChecker) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return
	}
	c.running = true
	c.done = make(chan struct{})
	go c.loop(ctx, c.done)
}

// Stop halts the background goroutine. Safe to call when not running.
func (c *PoolHealthChecker) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return
	}
	c.running = false
	close(c.done)
}

// Running reports whether the background goroutine is active.
func (c *PoolHealthChecker) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

func (c *PoolHealthChecker) loop(ctx context.Context, done <-chan struct{}) {
	ticker := time.NewTicker(c.checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			c.CheckAndPrune(ctx)
			c.lastCheckUnix.Store(time.Now().UnixNano())
			if c.enableAutoRefill && c.refill != nil && c.pool.Len() < c.minHealthyCount {
				if time.Since(c.lastRefillTime()) > c.refillCooldown {
					c.TriggerRefill(ctx)
				}
			}
		}
	}
}

// CheckAndPrune probes every proxy in the pool and removes the dead ones.
// It returns the number of healthy survivors and the number evicted.
func (c *PoolHealthChecker) CheckAndPrune(ctx context.Context) (healthy, evicted int) {
	current := c.pool.All()
	if len(current) == 0 {
		return 0, 0
	}
	survivors := make([]PoolProxy, 0, len(current))
	for _, p := range current {
		if c.probe(ctx, p, c.maxProbeTimeout) {
			survivors = append(survivors, p)
		} else {
			evicted++
		}
	}
	if evicted > 0 {
		c.pool.Replace(survivors)
	}
	c.totalEvicted.Add(int64(evicted))
	return len(survivors), evicted
}

// TriggerRefill harvests fresh proxies and appends the ones not already in
// the pool. It returns how many were added. The refill cooldown is enforced
// by the ticker loop; direct calls bypass it (mirrors Python's trigger_refill).
func (c *PoolHealthChecker) TriggerRefill(ctx context.Context) (added int) {
	if c.refill == nil {
		return 0
	}
	fresh := c.refill(ctx)
	if len(fresh) == 0 {
		return 0
	}
	existing := make(map[string]struct{}, c.pool.Len()+len(fresh))
	for _, p := range c.pool.All() {
		existing[p.Key()] = struct{}{}
	}
	for _, np := range fresh {
		if _, dup := existing[np.Key()]; dup {
			continue
		}
		c.pool.Add(np)
		existing[np.Key()] = struct{}{}
		added++
	}
	c.totalRefilled.Add(int64(added))
	c.lastRefillUnix.Store(time.Now().UnixNano())
	return added
}

// Stats returns a snapshot of the checker's counters.
func (c *PoolHealthChecker) Stats() HealthCheckerStats {
	return HealthCheckerStats{
		TotalEvicted:   c.totalEvicted.Load(),
		TotalRefilled:  c.totalRefilled.Load(),
		LastCheckTime:  unixToTime(c.lastCheckUnix.Load()),
		LastRefillTime: unixToTime(c.lastRefillUnix.Load()),
	}
}

func (c *PoolHealthChecker) lastRefillTime() time.Time {
	return unixToTime(c.lastRefillUnix.Load())
}

func unixToTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// DefaultProbeSingleProxy is the standard health probe: an HTTP GET to the
// Cloudflare trace endpoint routed through the candidate proxy. A 200 means
// alive; anything else (timeout, refused, non-200) means dead. Non-HTTP
// proxy schemes the stdlib cannot dial simply fail the probe, which matches
// the Python behavior of returning False on any exception.
func DefaultProbeSingleProxy(ctx context.Context, p PoolProxy, timeout time.Duration) bool {
	proto := strings.ToLower(strings.TrimSpace(p.Protocol))
	if proto == "" {
		proto = "http"
	}
	target := p.Key()
	if target == "" || target == ":" {
		return false
	}
	proxyURL, err := url.Parse(proto + "://" + target)
	if err != nil || proxyURL.Hostname() == "" {
		return false
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   timeout,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ProbeTargetURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
