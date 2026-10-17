// Package petaniproxy is a Go port of petani-proxy's core/server.py rotating
// forward-proxy gateway (dashboard HTML/JS and REST API intentionally omitted).
//
// It exposes a local HTTP/HTTPS CONNECT proxy endpoint that rotates every
// request across a pool of upstream proxies, sanitizes hop-by-hop headers,
// rotates User-Agents, and optionally pins clients to one upstream proxy via
// sticky sessions.
package petaniproxy

import (
	"net"
	"strconv"
	"sync"
	"time"
)

// Proxy describes a single upstream forward-proxy node.
//
// NOTE (shared-type candidate): sibling petani-proxy modules (fetcher,
// checker, exporter) will likely need this shape too. The coordinator should
// promote it to a shared package if/when those are ported.
type Proxy struct {
	IP          string
	Port        int
	Protocol    string // "http" (default); informational for now
	Username    string // optional upstream proxy auth
	Password    string // optional upstream proxy auth
	Country     string
	CountryCode string
	Anonymity   string
	LatencyMS   int64
}

// Addr returns the "ip:port" dial address of the upstream proxy.
func (p Proxy) Addr() string {
	return net.JoinHostPort(p.IP, strconv.Itoa(p.Port))
}

type stickySession struct {
	proxy     Proxy
	expiresAt time.Time
}

// DefaultSessionTTL mirrors server.py's session_ttl_sec = 600.
const DefaultSessionTTL = 600 * time.Second

// PoolManager manages the in-memory proxy pool, sticky sessions, and routing
// stats. It mirrors server.py's ProxyPoolManager. All methods are safe for
// concurrent use.
type PoolManager struct {
	mu         sync.Mutex
	proxies    []Proxy
	index      int
	total      uint64
	succeeded  uint64
	failed     uint64
	startedAt  time.Time
	sessions   map[string]*stickySession
	sessionTTL time.Duration
}

// NewPoolManager returns a PoolManager seeded with initial proxies.
func NewPoolManager(initial []Proxy) *PoolManager {
	return &PoolManager{
		proxies:    append([]Proxy(nil), initial...),
		startedAt:  time.Now(),
		sessions:   make(map[string]*stickySession),
		sessionTTL: DefaultSessionTTL,
	}
}

// SetProxies replaces the whole pool and resets the rotation index.
// Mirrors server.py's update_pool.
func (m *PoolManager) SetProxies(proxies []Proxy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proxies = append([]Proxy(nil), proxies...)
	m.index = 0
}

// GetAll returns a copy of the current pool.
func (m *PoolManager) GetAll() []Proxy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Proxy(nil), m.proxies...)
}

// Size returns the number of proxies in the pool.
func (m *PoolManager) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.proxies)
}

// SetSessionTTL changes how long a sticky session stays pinned.
func (m *PoolManager) SetSessionTTL(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionTTL = d
}

// GetRandom returns a random proxy from the pool.
// Mirrors server.py's get_random.
func (m *PoolManager) GetRandom() (Proxy, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.proxies) == 0 {
		return Proxy{}, false
	}
	return m.proxies[randIntN(len(m.proxies))], true
}

// ForceRotate advances the rotation index and returns the new current proxy.
// Mirrors server.py's force_rotate (does not count as a routed request).
func (m *PoolManager) ForceRotate() (Proxy, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.proxies) == 0 {
		return Proxy{}, false
	}
	m.index++
	return m.proxies[m.index%len(m.proxies)], true
}

// Next returns the next upstream proxy using round-robin rotation.
// If sessionID is non-empty, the client is pinned to one proxy for the
// session TTL (refreshed on every hit); expired sessions are purged.
// Mirrors server.py's get_next.
func (m *PoolManager) Next(sessionID string) (Proxy, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.proxies) == 0 {
		return Proxy{}, false
	}

	now := time.Now()
	for k, v := range m.sessions {
		if !v.expiresAt.After(now) {
			delete(m.sessions, k)
		}
	}

	if sessionID != "" {
		if sess, ok := m.sessions[sessionID]; ok && proxyInPool(sess.proxy, m.proxies) {
			sess.expiresAt = now.Add(m.sessionTTL)
			m.total++
			return sess.proxy, true
		}
		proxy := m.proxies[m.index%len(m.proxies)]
		m.index++
		m.sessions[sessionID] = &stickySession{proxy: proxy, expiresAt: now.Add(m.sessionTTL)}
		m.total++
		return proxy, true
	}

	proxy := m.proxies[m.index%len(m.proxies)]
	m.index++
	m.total++
	return proxy, true
}

// MarkResult records whether a routed request succeeded.
// Mirrors server.py's mark_result.
func (m *PoolManager) MarkResult(success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if success {
		m.succeeded++
	} else {
		m.failed++
	}
}

// PoolStats mirrors the dict returned by server.py's get_stats.
type PoolStats struct {
	UptimeSeconds        float64
	PoolSize             int
	TotalRouted          uint64
	Successful           uint64
	Failed               uint64
	SuccessRatePercent   float64
	ActiveStickySessions int
	CurrentIndex         int
	ActiveProxy          string // "ip:port" of the next rotation slot, "" if empty
	ActiveCountry        string
	ActiveCountryCode    string
}

// Stats returns a snapshot of pool routing statistics.
func (m *PoolManager) Stats() PoolStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	uptime := time.Since(m.startedAt).Seconds()
	rate := 100.0
	if m.total > 0 {
		rate = float64(m.succeeded) / float64(m.total) * 100
	}
	st := PoolStats{
		UptimeSeconds:        uptime,
		PoolSize:             len(m.proxies),
		TotalRouted:          m.total,
		Successful:           m.succeeded,
		Failed:               m.failed,
		SuccessRatePercent:   rate,
		ActiveStickySessions: len(m.sessions),
		CurrentIndex:         m.index,
	}
	if len(m.proxies) > 0 {
		cur := m.proxies[m.index%len(m.proxies)]
		st.ActiveProxy = cur.Addr()
		st.ActiveCountry = cur.Country
		st.ActiveCountryCode = cur.CountryCode
	}
	return st
}

func proxyInPool(p Proxy, pool []Proxy) bool {
	for _, q := range pool {
		if p == q {
			return true
		}
	}
	return false
}
