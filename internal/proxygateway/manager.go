// Package proxygateway manages the embedded petani-proxy gateway.
// Since 2026-10-17: Go-native implementation using internal/petaniproxy.
// No more Python subprocess — the gateway runs in-process.
package proxygateway

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"9router/proxy/internal/petaniproxy"
)

// Gateway address (fixed)
const (
	GatewayHost = "127.0.0.1"
	GatewayPort = 8888
	GatewayURL  = "http://127.0.0.1:8888"
)

// Harvest interval for background proxy refresh
const harvestInterval = 10 * time.Minute

// Manager controls the Go-native petani-proxy gateway.
type Manager struct {
	mu        sync.Mutex
	server    *petaniproxy.Server
	startTime time.Time
	enabled   bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// Status represents the current gateway state.
type Status struct {
	Running    bool       `json:"running"`
	Enabled    bool       `json:"enabled"`
	Available  bool       `json:"available"`
	PID        int        `json:"pid,omitempty"`
	Uptime     string     `json:"uptime,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	GatewayURL string     `json:"gatewayUrl"`
	ProxyCount int        `json:"proxyCount,omitempty"`
}

// NewManager creates a gateway manager.
func NewManager() *Manager {
	return &Manager{}
}

// NewDefaultManager creates a manager with default settings.
func NewDefaultManager() *Manager {
	return NewManager()
}

// IsAvailable always returns true for the Go-native gateway.
func (m *Manager) IsAvailable() bool {
	return true
}

// isPortInUse checks if a TCP port is listening on the given host.
func isPortInUse(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Start launches the Go-native gateway.
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.server != nil {
		m.enabled = true
		return nil // Already running
	}

	// If something else is on 8888 (e.g. old Python gateway), adopt it
	if isPortInUse(GatewayHost, GatewayPort) {
		m.enabled = true
		return nil
	}

	// Create the Go server
	srv := petaniproxy.NewServer(
		fmt.Sprintf("%s:%d", GatewayHost, GatewayPort),
		petaniproxy.WithDialTimeout(10*time.Second),
		petaniproxy.WithMaxAttempts(3),
	)

	if err := srv.Start(); err != nil {
		return fmt.Errorf("failed to start gateway: %w", err)
	}

	// Start background harvester
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(1)
	go m.harvestLoop(ctx, srv)

	m.server = srv
	m.startTime = time.Now()
	m.enabled = true

	return nil
}

// harvestLoop periodically fetches and validates proxies in the background.
func (m *Manager) harvestLoop(ctx context.Context, srv *petaniproxy.Server) {
	defer m.wg.Done()

	// Initial harvest (don't block startup)
	go m.doHarvest(ctx, srv)

	ticker := time.NewTicker(harvestInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.doHarvest(ctx, srv)
		}
	}
}

// doHarvest fetches proxy candidates, validates them, and updates the pool.
func (m *Manager) doHarvest(ctx context.Context, srv *petaniproxy.Server) {
	hctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Fetch candidates
	candidates, err := petaniproxy.FetchProxies(hctx, petaniproxy.FetchOptions{})
	if err != nil || len(candidates) == 0 {
		return
	}

	// Convert to ProxyCandidate for checking
	var toCheck []petaniproxy.ProxyCandidate
	for _, c := range candidates {
		toCheck = append(toCheck, petaniproxy.ProxyCandidate{
			IP:       c.IP,
			Port:     c.Port,
			Protocol: c.Protocol,
		})
	}

	// Validate
	live := petaniproxy.CheckProxiesPool(hctx, toCheck, petaniproxy.CheckPoolOptions{})
	if len(live) == 0 {
		return
	}

	// Update server pool
	var proxies []petaniproxy.Proxy
	for _, lp := range live {
		proxies = append(proxies, petaniproxy.Proxy{
			IP:       lp.IP,
			Port:     lp.Port,
			Protocol: lp.Protocol,
		})
	}
	srv.SetProxies(proxies)
}

// Stop terminates the gateway.
func (m *Manager) Stop() error {
	m.mu.Lock()
	m.enabled = false

	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}

	if m.server != nil {
		_ = m.server.Stop()
		m.server = nil
	}
	m.mu.Unlock()

	// Wait for harvester to finish (without holding the lock)
	m.wg.Wait()

	return nil
}

// Status returns the current gateway status.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := Status{
		Enabled:    m.enabled,
		Available:  true,
		GatewayURL: GatewayURL,
	}

	if m.server != nil {
		s.Running = true
		startTime := m.startTime
		s.StartedAt = &startTime
		s.Uptime = time.Since(m.startTime).Round(time.Second).String()
		if pool := m.server.Pool(); pool != nil {
			s.ProxyCount = pool.Size()
		}
		return s
	}

	// Fall back to port check (externally started gateway)
	if isPortInUse(GatewayHost, GatewayPort) {
		s.Running = true
	}

	return s
}

// IsEnabled returns whether the gateway is enabled.
func (m *Manager) IsEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled
}

// SetEnabled sets the enabled flag.
func (m *Manager) SetEnabled(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled = enabled
}

// PoolInfo contains the current proxy pool data.
type PoolInfo struct {
	Proxies []petaniproxy.Proxy      `json:"proxies"`
	Stats   petaniproxy.PoolStats    `json:"stats"`
	Count   int                      `json:"count"`
}

// GetPool returns the current proxy pool info.
// Returns nil if the gateway is not running its own server.
func (m *Manager) GetPool() *PoolInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.server == nil {
		return nil
	}

	pool := m.server.Pool()
	if pool == nil {
		return nil
	}

	proxies := pool.GetAll()
	return &PoolInfo{
		Proxies: proxies,
		Stats:   pool.Stats(),
		Count:   len(proxies),
	}
}

// TriggerHarvest runs a manual proxy harvest cycle.
// Returns the number of live proxies found.
func (m *Manager) TriggerHarvest() (int, error) {
	m.mu.Lock()
	srv := m.server
	m.mu.Unlock()

	if srv == nil {
		return 0, fmt.Errorf("gateway not running")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	m.doHarvest(ctx, srv)

	if pool := srv.Pool(); pool != nil {
		return pool.Size(), nil
	}
	return 0, nil
}
