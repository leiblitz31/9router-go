// Package proxygateway manages the embedded petani-proxy gateway subprocess.
// It provides start/stop/status control for the local rotating proxy gateway
// that 9Router uses as a proxy pool for IP rotation.
package proxygateway

import (
	"fmt"
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
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cmd != nil && m.cmd.Process != nil {
		// Already running, check if alive
		if err := m.cmd.Process.Signal(syscall.Signal(0)); err == nil {
			return fmt.Errorf("gateway already running (PID %d)", m.cmd.Process.Pid)
		}
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
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.enabled = false

	if m.cmd == nil || m.cmd.Process == nil {
		return nil // Not running
	}

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

// Status returns the current gateway status.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := Status{
		Enabled:    m.enabled,
		GatewayURL: GatewayURL,
	}

	if m.cmd != nil && m.cmd.Process != nil {
		if err := m.cmd.Process.Signal(syscall.Signal(0)); err == nil {
			s.Running = true
			s.PID = m.cmd.Process.Pid
			s.StartedAt = m.startTime
			s.Uptime = time.Since(m.startTime).Round(time.Second).String()
		} else {
			// Process died
			m.cmd = nil
		}
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
