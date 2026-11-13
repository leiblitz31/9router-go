package translator

import (
	json "encoding/json/v2"
	"testing"
)

func TestAnchorClaudeCache_PreservesLastSystemAnchor(t *testing.T) {
	tests := []struct {
		name   string
		system string
	}{
		{"single unmarked", `[{"type":"text","text":"a"}]`},
		{"single 5m", `[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]`},
		{"downgrades client 1h", `[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := AnchorClaudeCache([]byte(`{"system":` + tt.system + `,"messages":[{"role":"user","content":"test"}]}`))
			var req map[string]any
			if err := json.Unmarshal(out, &req); err != nil {
				t.Fatal(err)
			}
			sys := req["system"].([]any)
			cc, ok := sys[len(sys)-1].(map[string]any)["cache_control"].(map[string]any)
			if !ok || cc["ttl"] != nil {
				t.Errorf("last block should have the 5m default, got %v", cc)
			}
		})
	}
}

// TestAnchorClaudeCacheWithTTL pins the operator knob: "1h" moves both head
// anchors to the 1h window, and the default wrapper keeps them at 5m.
func TestAnchorClaudeCacheWithTTL(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"sys"}],"tools":[{"name":"a","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`)
	for _, tc := range []struct {
		name string
		ttl  string
		want any
	}{
		{"default", "", nil},
		{"explicit 5m", ClaudeCacheTTL5m, nil},
		{"1h", ClaudeCacheTTL1h, "1h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := AnchorClaudeCacheWithTTL(body, tc.ttl)
			var req map[string]any
			if err := json.Unmarshal(out, &req); err != nil {
				t.Fatal(err)
			}
			sys := req["system"].([]any)[0].(map[string]any)["cache_control"].(map[string]any)
			tool := req["tools"].([]any)[0].(map[string]any)["cache_control"].(map[string]any)
			if sys["ttl"] != tc.want || tool["ttl"] != tc.want {
				t.Errorf("ttl %q: system=%v tool=%v, want %v", tc.ttl, sys["ttl"], tool["ttl"], tc.want)
			}
		})
	}
}
