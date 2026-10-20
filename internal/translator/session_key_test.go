package translator

import (
	"sync"
	"testing"
)

// TestNewSessionKeyIsUniqueUnderConcurrency pins the property the crash
// depended on: two live requests must never receive the same session key.
//
// The key used to be built from time.Now().UnixNano() alone. On Windows the
// wall clock ticks at ~15.6 ms, so 200 concurrent calls produced only two
// distinct nanosecond values — 104 requests shared one key, therefore one
// *StreamState, therefore one map written from two goroutines. That crashed
// the gateway with "concurrent map writes" under load.
func TestNewSessionKeyIsUniqueUnderConcurrency(t *testing.T) {
	const goroutines = 200
	keys := make([]string, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			keys[idx] = NewSessionKey("stream")
		}(i)
	}
	wg.Wait()

	seen := make(map[string]int, goroutines)
	for _, k := range keys {
		if k == "" {
			t.Fatal("empty session key")
		}
		seen[k]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Fatalf("session key %q handed out %d times", k, n)
		}
	}
	if len(seen) != goroutines {
		t.Fatalf("distinct keys = %d, want %d", len(seen), goroutines)
	}
}

// TestNewSessionKeyKeepsPrefix pins that the pruner and the log lines can
// still tell a stream session from a gemini one.
func TestNewSessionKeyKeepsPrefix(t *testing.T) {
	key := NewSessionKey("gemini-stream")
	if len(key) < len("gemini-stream-") || key[:len("gemini-stream-")] != "gemini-stream-" {
		t.Fatalf("key %q lost its prefix", key)
	}
}
