package proxy

import (
	"testing"
	"time"
)

// The translated-session memo must expire: after the TTL a conversation maps
// to a freshly minted id instead of reusing the stale one.
func TestTranslateOpenCodeSessionIDTTLExpiry(t *testing.T) {
	old := opencodeTranslatedSessionTTL
	opencodeTranslatedSessionTTL = time.Millisecond
	defer func() { opencodeTranslatedSessionTTL = old }()

	first := TranslateOpenCodeSessionID("conv-ttl-expiry", "")
	time.Sleep(5 * time.Millisecond)
	if again := TranslateOpenCodeSessionID("conv-ttl-expiry", ""); again == first {
		t.Errorf("expired memo entry was reused: %q", first)
	}
}
