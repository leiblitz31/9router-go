package proxygateway

import (
	"testing"
	"time"
)

func TestManagerStartStop(t *testing.T) {
	m := NewManager()

	// Start
	if err := m.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Should be running (either own server or adopted existing gateway)
	s := m.Status()
	if !s.Running {
		t.Error("expected running after Start()")
	}
	if !s.Enabled {
		t.Error("expected enabled after Start()")
	}
	if !s.Available {
		t.Error("expected available=true for Go-native gateway")
	}

	// Starting again should be idempotent
	if err := m.Start(); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}

	// Stop
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if m.IsEnabled() {
		t.Error("expected disabled after Stop()")
	}
}

func TestManagerStatusFields(t *testing.T) {
	m := NewManager()

	if err := m.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer m.Stop()

	// Give server a moment to start
	time.Sleep(100 * time.Millisecond)

	s := m.Status()
	if s.GatewayURL != GatewayURL {
		t.Errorf("GatewayURL = %q, want %q", s.GatewayURL, GatewayURL)
	}
	// StartedAt is only set when we own the server (not when adopting existing)
	// Just verify the field exists in the struct
	_ = s.StartedAt
}
