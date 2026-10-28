package executor

import (
	"encoding/json"
	"encoding/json/jsontext"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeepSeekEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"single char", "a", 1},
		{"one token boundary", "abcd", 1},
		{"over boundary", "abcde", 2},
		{"two tokens", "abcdefgh", 2},
		{"one CJK rune", "中", 2},
		{"three CJK runes", "中文中", 5},
		{"mixed ascii and CJK", "ab中", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deepSeekEstimateTokens(tc.text); got != tc.want {
				t.Errorf("deepSeekEstimateTokens(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

func TestDeepSeekBuildUsage(t *testing.T) {
	usage := deepSeekBuildUsage("abcd", "abcdefgh", "")
	if usage["prompt_tokens"] != 1 || usage["completion_tokens"] != 2 || usage["total_tokens"] != 3 {
		t.Errorf("usage = %v, want prompt 1 completion 2 total 3", usage)
	}
	if _, ok := usage["completion_tokens_details"]; ok {
		t.Errorf("usage without reasoning must not carry completion_tokens_details: %v", usage)
	}

	usage = deepSeekBuildUsage("abcd", "abcdefgh", "abcd")
	if usage["prompt_tokens"] != 1 || usage["completion_tokens"] != 3 || usage["total_tokens"] != 4 {
		t.Errorf("usage with reasoning = %v, want prompt 1 completion 3 total 4", usage)
	}
	details, _ := usage["completion_tokens_details"].(map[string]any)
	if details["reasoning_tokens"] != 1 {
		t.Errorf("reasoning_tokens = %v, want 1", details["reasoning_tokens"])
	}
}

func TestDeepSeekWriteJSONReportsEstimatedUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	req := &Request{}
	message := map[string]any{"role": "assistant", "content": "hello world"}
	if err := deepSeekWriteJSON(rec, req, "m", message, "abcd"); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Usage["prompt_tokens"] != float64(1) || out.Usage["completion_tokens"] != float64(3) || out.Usage["total_tokens"] != float64(4) {
		t.Errorf("usage = %v, want prompt 1 completion 3 total 4", out.Usage)
	}
}

func TestDeepSeekTurnPromptRetryHint(t *testing.T) {
	var spec deepSeekToolSpec
	spec.Function.Name = "get_x"
	spec.Function.Description = "d"
	env := deepSeekRequestEnvelope{
		Model:    "m",
		Messages: []deepSeekMessage{{Role: "user", Content: jsontext.Value(`"hi"`)}},
		Tools:    []deepSeekToolSpec{spec},
	}
	got := deepSeekTurnPrompt(env, &Request{}, deepSeekStealth{})
	if !strings.Contains(got, "retry once with a different or reversed combination") {
		t.Errorf("tool prompt lacks the retry hint:\n%s", got)
	}

	env.Tools = nil
	got = deepSeekTurnPrompt(env, &Request{}, deepSeekStealth{})
	if strings.Contains(got, "retry once with a different or reversed combination") {
		t.Errorf("plain prompt must not carry the retry hint:\n%s", got)
	}
}
