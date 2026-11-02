package executor

import (
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strings"
	"testing"
)

func TestQwenWebBXUA(t *testing.T) {
	tests := []struct{ name string }{{"generates a token"}, {"second token differs"}}
	seen := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok := qwenWebBXUA()
			if !strings.HasPrefix(tok, "2536!") {
				t.Fatalf("bx-ua prefix = %q, want 2536!", tok[:5])
			}
			if seen[tok] {
				t.Fatalf("bx-ua repeated across generations")
			}
			seen[tok] = true
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(tok, "2536!"))
			if err != nil {
				t.Fatalf("bx-ua payload is not base64: %v", err)
			}
			var fp qwenWebFingerprint
			if err := json.Unmarshal(raw, &fp); err != nil {
				t.Fatalf("bx-ua payload is not fingerprint JSON: %v", err)
			}
			if fp.Platform != "Win32" || fp.Timestamp == 0 || fp.Canvas == "" {
				t.Errorf("fingerprint missing fields: %+v", fp)
			}
		})
	}
}

func TestQwenWebIsRiskControl(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"rgv587 refusal", `{"ret":["FAIL_SYS_USER_VALIDATE","RGV587_ERROR::SM::x"],"data":{"url":"https://chat.qwen.ai/_____tmd_____/punish"}}`, true},
		{"plain success", `{"success":true,"data":{"id":"abc"}}`, false},
		{"sse frame", `{"choices":[{"delta":{"phase":"answer","content":"pong"}}]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qwenWebIsRiskControl([]byte(tt.body)); got != tt.want {
				t.Errorf("qwenWebIsRiskControl = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestQwenWebAccumulatorPhases replays the frame shapes captured live from
// chat.qwen.ai on 2026-11-02: a response.created frame, two identical
// thinking_summary snapshot frames (the second must contribute nothing),
// answer deltas, and the finished frame.
func TestQwenWebAccumulatorPhases(t *testing.T) {
	frames := []string{
		`{"response.created":{"chat_id":"c1","response_id":"r1"}}`,
		`{"choices":[{"delta":{"role":"assistant","content":"","phase":"thinking_summary","extra":{"summary_thought":{"content":["I recognise a ping."]}},"status":"typing"}}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`,
		`{"choices":[{"delta":{"role":"assistant","content":"","phase":"thinking_summary","extra":{"summary_thought":{"content":["I recognise a ping."]}},"status":"typing"}}]}`,
		`{"choices":[{"delta":{"role":"assistant","content":"","phase":"thinking_summary","extra":{"summary_thought":{"content":["I recognise a ping. I will answer pong."]}},"status":"typing"}}]}`,
		`{"choices":[{"delta":{"content":"Pong","phase":"answer","status":"typing"}}]}`,
		`{"choices":[{"delta":{"content":"!","phase":"answer","status":"typing"}}],"usage":{"input_tokens":10,"output_tokens":9,"total_tokens":19,"output_tokens_details":{"reasoning_tokens":4,"text_tokens":5}}}`,
		`{"choices":[{"delta":{"content":"","role":"assistant","status":"finished","phase":"answer"}}]}`,
	}
	acc := &qwenWebAccumulator{}
	var reasoning, answer strings.Builder
	for _, f := range frames {
		rd, ad := acc.apply([]byte(f))
		reasoning.WriteString(rd)
		answer.WriteString(ad)
	}
	if got := reasoning.String(); got != "I recognise a ping. I will answer pong." {
		t.Errorf("reasoning = %q", got)
	}
	if got := answer.String(); got != "Pong!" {
		t.Errorf("answer = %q", got)
	}
	if acc.usage == nil || acc.usage.TotalTokens != 19 || acc.usage.OutputDetails.ReasoningTokens != 4 {
		t.Errorf("usage = %+v, want last frame's usage", acc.usage)
	}
	if !acc.finished {
		t.Errorf("finished flag not set")
	}
}

func TestQwenWebBuildMessagesFlatten(t *testing.T) {
	tests := []struct {
		name    string
		in      []qwenWebInputMsg
		wantSub []string
	}{
		{"lone user message goes out verbatim", []qwenWebInputMsg{
			{Role: "user", Content: jsontext.Value(`"ping"`)},
		}, []string{"ping"}},
		{"history flattens to a transcript", []qwenWebInputMsg{
			{Role: "user", Content: jsontext.Value(`"Ingat angka 4217"`)},
			{Role: "assistant", Content: jsontext.Value(`"Baik, 4217."`)},
			{Role: "user", Content: jsontext.Value(`"Berapa angkanya?"`)},
		}, []string{"User: Ingat angka 4217", "Assistant: Baik, 4217.", "User: Berapa angkanya?"}},
		{"system folds in as an unlabeled head block", []qwenWebInputMsg{
			{Role: "system", Content: jsontext.Value(`"Jawab singkat."`)},
			{Role: "user", Content: jsontext.Value(`"halo"`)},
		}, []string{"Jawab singkat.", "User: halo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := qwenWebBuildMessages(tt.in, "qwen3.7-plus", true)
			if len(msgs) != 1 {
				t.Fatalf("wire messages = %d, want exactly 1", len(msgs))
			}
			m := msgs[0]
			if m.Role != "user" {
				t.Errorf("wire role = %q, want user", m.Role)
			}
			for _, sub := range tt.wantSub {
				if !strings.Contains(m.Content, sub) {
					t.Errorf("content %q missing %q", m.Content, sub)
				}
			}
			if m.FeatureConfig == nil || !m.FeatureConfig.ThinkingEnabled {
				t.Errorf("feature_config = %+v, want thinking enabled", m.FeatureConfig)
			}
			if len(m.Models) != 1 || m.Models[0] != "qwen3.7-plus" {
				t.Errorf("models = %v", m.Models)
			}
		})
	}
}

func TestQwenWebThinkingEnabled(t *testing.T) {
	off := false
	on := true
	tests := []struct {
		name string
		env  qwenWebRequestEnvelope
		want bool
	}{
		{"default follows the site client (on)", qwenWebRequestEnvelope{}, true},
		{"reasoning_effort none disables", qwenWebRequestEnvelope{ReasoningEffort: "none"}, false},
		{"reasoning_effort high enables", qwenWebRequestEnvelope{ReasoningEffort: "high"}, true},
		{"explicit false wins", qwenWebRequestEnvelope{EnableThinking: &off, ReasoningEffort: "high"}, false},
		{"explicit true wins over none", qwenWebRequestEnvelope{EnableThinking: &on, ReasoningEffort: "none"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qwenWebThinkingEnabled(tt.env); got != tt.want {
				t.Errorf("thinking = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestQwenWebTextContentParts(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain string", `"halo"`, "halo"},
		{"parts array", `[{"type":"text","text":"satu "},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"dua"}]`, "satu dua"},
		{"empty", `""`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qwenWebTextContent(jsontext.Value(tt.raw)); got != tt.want {
				t.Errorf("text = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQwenWebCredentialFor(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjk5OTk5OTk5OTl9.sig"
	tests := []struct {
		name     string
		apiKey   string
		connData map[string]any
		want     qwenWebCredential
	}{
		{"no key is guest", "", nil, qwenWebCredential{}},
		{"guest placeholder is guest", "guest", nil, qwenWebCredential{}},
		{"bare jwt is login", jwt, nil, qwenWebCredential{Token: jwt, LoggedIn: true}},
		{"value wrapper unwraps", `{"value":"` + jwt + `"}`, nil, qwenWebCredential{Token: jwt, LoggedIn: true}},
		{"cookie string yields token", "cna=x1; token=" + jwt + "; sca=y", nil, qwenWebCredential{Token: jwt, Cookie: "cna=x1; token=" + jwt + "; sca=y", LoggedIn: true}},
		{"connData cookie joins token", jwt, map[string]any{"qwenCookie": "token=" + jwt}, qwenWebCredential{Token: jwt, Cookie: "token=" + jwt, LoggedIn: true}},
		{"cookie alone is not login", "", map[string]any{"qwenCookie": "cna=x1"}, qwenWebCredential{Cookie: "cna=x1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := qwenWebCredentialFor(&Request{APIKey: tt.apiKey, ConnData: tt.connData})
			if got != tt.want {
				t.Errorf("credential = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestQwenWebJWTExpiry(t *testing.T) {
	// payload {"exp":9999999999} and {"exp":1}, base64url without padding
	future := "h.eyJleHAiOjk5OTk5OTk5OTl9.s"
	past := "h.eyJleHAiOjF9.s"
	if exp, ok := qwenWebJWTExpiry(future); !ok || exp.Year() < 2200 {
		t.Errorf("future exp = %v ok=%v", exp, ok)
	}
	if exp, ok := qwenWebJWTExpiry(past); !ok || exp.Year() != 1970 {
		t.Errorf("past exp = %v ok=%v", exp, ok)
	}
	if _, ok := qwenWebJWTExpiry("dummy-token-not-a-jwt"); ok {
		t.Errorf("non-JWT reported parseable")
	}
}

func TestQwenWebFindMintedToken(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"data.token", `{"data":{"token":"T1"}}`, "T1"},
		{"data.access_token", `{"data":{"access_token":"T2"}}`, "T2"},
		{"nested data.data.token", `{"data":{"data":{"token":"T3"}}}`, "T3"},
		{"top-level token", `{"token":"T4"}`, "T4"},
		{"no token", `{"success":true}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qwenWebFindMintedToken([]byte(tt.body)); got != tt.want {
				t.Errorf("minted = %q, want %q", got, tt.want)
			}
		})
	}
}
