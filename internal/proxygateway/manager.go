// Package proxygateway manages the embedded petani-proxy gateway subprocess.
// It provides start/stop/status control for the local rotating proxy gateway
// that 9Router uses as a proxy pool for IP rotation.
package proxygateway

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Gateway address (fixed)
const (
	GatewayHost = "127.0.0.1"
	GatewayPort = 8888
	GatewayURL  = "http://127.0.0.1:8888"
)

// Manager controls the petani-proxy gateway subprocess.
type Manager struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	startTime time.Time
	enabled   bool // whether gateway should be running (persisted via DB)
	scriptDir string
	pythonBin string
}

// Status represents the current gateway state.
type Status struct {
	Running    bool      `json:"running"`
	Enabled    bool      `json:"enabled"`
	Available  bool      `json:"available"`
	PID        int       `json:"pid,omitempty"`
	Uptime     string    `json:"uptime,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	GatewayURL string    `json:"gatewayUrl"`
}

// NewManager creates a gateway manager.
// scriptDir is the petani-proxy installation directory.
// pythonBin is the Python interpreter (venv).
func NewManager(scriptDir, pythonBin string) *Manager {
	return &Manager{
		scriptDir: scriptDir,
		pythonBin: pythonBin,
		enabled:   false,
	}
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

// defaultPaths returns the default petani-proxy paths.
func defaultPaths() (scriptDir, pythonBin string) {
	home, _ := os.UserHomeDir()
	scriptDir = filepath.Join(home, "vps-persist", "petani-proxy")
	pythonBin = filepath.Join(scriptDir, "venv", "bin", "python")
	return scriptDir, pythonBin
}

// NewDefaultManager creates a manager with default paths.
func NewDefaultManager() *Manager {
	sd, pb := defaultPaths()
	return NewManager(sd, pb)
}

// IsAvailable checks if the gateway can be started (script + python exist).
func (m *Manager) IsAvailable() bool {
	mainPy := filepath.Join(m.scriptDir, "main.py")
	if _, err := os.Stat(mainPy); err != nil {
		return false
	}
	if _, err := os.Stat(m.pythonBin); err != nil {
		return false
	}
	return true
}

// Start launches the gateway subprocess.
// If the gateway is already running (port 8888 in use), it is adopted
// as running instead of failing.
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cmd != nil && m.cmd.Process != nil {
		// Already running via this manager, check if alive
		if err := m.cmd.Process.Signal(syscall.Signal(0)); err == nil {
			m.enabled = true
			return nil // Already running, treat as success
		}
		// Process died, clear it
		m.cmd = nil
	}

	// Check if port 8888 is already in use (externally started gateway)
	if isPortInUse(GatewayHost, GatewayPort) {
		m.enabled = true
		// Try to find the PID for status display
		m.cmd = nil // We don't own it, but mark as enabled
		return nil
	}

	if !m.IsAvailable() {
		return fmt.Errorf("petani-proxy not installed at %s", m.scriptDir)
	}

	cmd := exec.Command(m.pythonBin, "main.py", "--daemon-gateway")
	cmd.Dir = m.scriptDir
	// Inherit proxy env for egress
	cmd.Env = os.Environ()

	// Detach from parent
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Log to file
	logFile, err := os.OpenFile(
		filepath.Join(m.scriptDir, "petani-proxy.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644,
	)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start gateway: %w", err)
	}

	m.cmd = cmd
	m.startTime = time.Now()
	m.enabled = true

	return nil
}

// Stop terminates the gateway subprocess.
// Also kills externally-started gateways (by port) if manager didn't start it.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.enabled = false

	if m.cmd != nil && m.cmd.Process != nil {
		// Try graceful shutdown first
		_ = m.cmd.Process.Signal(syscall.SIGTERM)

		// Wait a bit, then force kill
		done := make(chan struct{})
		go func() {
			_, _ = m.cmd.Process.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Exited cleanly
		case <-time.After(5 * time.Second):
			// Force kill
			_ = m.cmd.Process.Kill()
			<-done
		}

		m.cmd = nil
		return nil
	}

	// No manager-owned process; try to kill by port (external gateway).
	// We use pkill with a pattern matching the gateway's Python process.
	// This is best-effort; failure is not an error.
	if isPortInUse(GatewayHost, GatewayPort) {
		_ = exec.Command("pkill", "-f", "petani-proxy.*daemon-gateway").Run()
		// Also try the start script pattern
		_ = exec.Command("pkill", "-f", "[m]ain.py --daemon-gateway").Run()
	}

	return nil
}

// Status returns the current gateway status.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := Status{
		Enabled:    m.enabled,
		GatewayURL: GatewayURL,
	}

	// Check manager-owned process first
	if m.cmd != nil && m.cmd.Process != nil {
		if err := m.cmd.Process.Signal(syscall.Signal(0)); err == nil {
			s.Running = true
			s.PID = m.cmd.Process.Pid
			s.StartedAt = m.startTime
			s.Uptime = time.Since(m.startTime).Round(time.Second).String()
			return s
		}
		// Process died
		m.cmd = nil
	}

	// Fall back to port check (externally started gateway)
	if isPortInUse(GatewayHost, GatewayPort) {
		s.Running = true
		// PID unknown for external process, but mark as running
		// so the dashboard shows correct state
	}

	return s
}

// IsEnabled returns whether the gateway is enabled.
func (m *Manager) IsEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.enabled
}

// SetEnabled sets the enabled flag (for persistence).
func (m *Manager) SetEnabled(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enabled = enabled
}
