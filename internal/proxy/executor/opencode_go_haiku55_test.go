package executor

import (
	"strings"
	"testing"

	json "encoding/json/v2"
)

// Upstream #4727: claude-haiku-5-5 is served by OpenCode Go's
// /zen/go/v1/messages only — /chat/completions answers
// "ModelProtocolUnsupported" — so it must ride the Claude Messages lane
// beside minimax/qwen, not the default OpenAI route.

func TestForwardOpencodeGo_Haiku55RoutesToMessages(t *testing.T) {
	if !opencodeGoMessagesModels["claude-haiku-5-5"] {
		t.Fatal("claude-haiku-5-5 must route to the /messages lane")
	}
	if opencodeGoMessagesModels["glm-5.3-flash"] {
		t.Error("glm-5.3-flash is served by /chat/completions and must not move")
	}
}

func TestIsAnthropicServingProvider(t *testing.T) {
	for _, tc := range []struct {
		provider string
		want     bool
	}{
		{"claude", true},
		{"anthropic", true},
		{"anthropic-compatible-acme", true},
		{"opencode-go", false},
		{"opencode-zen", false},
		{"minimax-code", false},
		{"", false},
	} {
		if got := isAnthropicServingProvider(tc.provider); got != tc.want {
			t.Errorf("isAnthropicServingProvider(%q) = %v, want %v", tc.provider, got, tc.want)
		}
	}
}

// Only Anthropic serves context_management; every other Claude-wire provider
// answers 400 "Extra inputs are not permitted", and a claude→claude request
// skips body rebuilding so the field rides straight through.
func TestEnsureMessagesFor_StripsContextManagementOffAnthropic(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-5-5","context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":256}`)

	var got map[string]any
	if err := json.Unmarshal(ensureMessagesFor(body, "opencode-go", "claude-haiku-5-5"), &got); err != nil {
		t.Fatalf("unmarshal opencode-go body: %v", err)
	}
	if _, present := got["context_management"]; present {
		t.Error("context_management must be stripped for a non-Anthropic provider")
	}

	var native map[string]any
	if err := json.Unmarshal(ensureMessagesFor(body, "claude", "claude-haiku-5-5"), &native); err != nil {
		t.Fatalf("unmarshal claude body: %v", err)
	}
	if _, present := native["context_management"]; !present {
		t.Error("context_management must survive for provider claude")
	}
}

// haiku-5-5 rejects thinking.type.enabled — the shape the global
// *claude-haiku* pattern sends — with 400, so the provider override must
// select claude-adaptive (thinking.type adaptive + output_config.effort).
func TestEnsureMessagesFor_Haiku55EmitsAdaptiveThinking(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-5-5","reasoning_effort":"high",` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"max_tokens":4096}`)

	var got map[string]any
	if err := json.Unmarshal(ensureMessagesFor(body, "opencode-go", "claude-haiku-5-5"), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	thinking, _ := got["thinking"].(map[string]any)
	if thinking == nil {
		t.Fatal("thinking block missing")
	}
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking.type = %v, want adaptive (never \"enabled\")", thinking["type"])
	}
	if _, present := thinking["budget_tokens"]; present {
		t.Error("adaptive thinking must not carry budget_tokens")
	}
	outCfg, _ := got["output_config"].(map[string]any)
	if outCfg == nil {
		t.Fatal("output_config missing; adaptive thinking declares effort there")
	}
	if outCfg["effort"] != "high" {
		t.Errorf("output_config.effort = %v, want high", outCfg["effort"])
	}
	if _, present := got["reasoning_effort"]; present {
		t.Error("reasoning_effort is OpenAI-only and must be stripped")
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), `"type":"enabled"`) {
		t.Errorf("thinking.type=enabled leaked into the Claude body: %s", raw)
	}
}
