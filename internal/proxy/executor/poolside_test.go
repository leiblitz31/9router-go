package executor

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"9router/proxy/internal/providers"
)

func TestPoolsideUpstreamID(t *testing.T) {
	cases := map[string]string{
		"laguna-s-2.1":          "poolside/laguna-s-2.1",
		"laguna-xs-2.1":         "poolside/laguna-xs-2.1",
		"poolside/laguna-s-2.1": "poolside/laguna-s-2.1", // already scoped: untouched
		"":                      "",
	}
	for in, want := range cases {
		if got := poolsideUpstreamID(in); got != want {
			t.Errorf("poolsideUpstreamID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStripPoolsideReasoningCollision pins the upstream serde-alias
// collision: a message carrying both `reasoning` and `reasoning_content`
// is rejected by Poolside with 400 "duplicate field `reasoning_content`".
// Only the colliding `reasoning` key is dropped; every other shape passes
// through untouched.
func TestStripPoolsideReasoningCollision(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantStrip bool
	}{
		{
			name:      "both keys on assistant drops reasoning",
			body:      `{"model":"laguna-s-2.1","messages":[{"role":"assistant","content":"ok","reasoning_content":"t","reasoning":"t"}]}`,
			wantStrip: true,
		},
		{
			name:      "both keys on user drops reasoning",
			body:      `{"model":"laguna-s-2.1","messages":[{"role":"user","content":"hi","reasoning_content":"t","reasoning":"t"}]}`,
			wantStrip: true,
		},
		{
			name:      "reasoning_content only untouched",
			body:      `{"model":"laguna-s-2.1","messages":[{"role":"assistant","content":"ok","reasoning_content":"t"}]}`,
			wantStrip: false,
		},
		{
			name:      "reasoning only untouched",
			body:      `{"model":"laguna-s-2.1","messages":[{"role":"assistant","content":"ok","reasoning":"t"}]}`,
			wantStrip: false,
		},
		{
			name:      "no messages untouched",
			body:      `{"model":"laguna-s-2.1"}`,
			wantStrip: false,
		},
		{
			name:      "invalid json untouched",
			body:      `{not json`,
			wantStrip: false,
		},
		{
			name:      "non-object message skipped, colliding one stripped",
			body:      `{"model":"laguna-s-2.1","messages":["hi",{"role":"assistant","content":"ok","reasoning_content":"t","reasoning":"t"}]}`,
			wantStrip: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := stripPoolsideReasoningCollision([]byte(tc.body))
			if ok != tc.wantStrip {
				t.Fatalf("stripPoolsideReasoningCollision() changed = %v, want %v", ok, tc.wantStrip)
			}
			if !tc.wantStrip {
				return
			}
			var env map[string]any
			if err := json.Unmarshal(out, &env); err != nil {
				t.Fatalf("stripped body is not valid JSON: %v", err)
			}
			msgs, _ := env["messages"].([]any)
			last, _ := msgs[len(msgs)-1].(map[string]any)
			if _, has := last["reasoning"]; has {
				t.Errorf("stripped body still carries reasoning key: %s", out)
			}
			if _, has := last["reasoning_content"]; !has {
				t.Errorf("stripped body lost reasoning_content: %s", out)
			}
			if last["content"] != "ok" && last["content"] != "hi" {
				t.Errorf("stripped body lost content: %s", out)
			}
		})
	}
}

// TestForwardPoolsideStripsReasoningCollision drives a colliding history
// message through the executor against a fake upstream and asserts the wire
// body carries reasoning_content without reasoning, while the model rewrite
// still applies in the same request.
func TestForwardPoolsideStripsReasoningCollision(t *testing.T) {
	var wireBody []byte
	var wireModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		wireBody = raw
		var env map[string]any
		_ = json.Unmarshal(raw, &env)
		wireModel, _ = env["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)

	rec := httptest.NewRecorder()
	err := ForwardPoolside(rec, &Request{
		Ctx:    t.Context(),
		Client: srv.Client(),
		Config: &providers.ProviderConfig{BaseURL: srv.URL + "/v1/chat/completions"},
		APIKey: "sky_test",
		Body:   []byte(`{"model":"laguna-s-2.1","messages":[{"role":"assistant","content":"ok","reasoning_content":"t","reasoning":"t"},{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("ForwardPoolside() error = %v", err)
	}
	if wireModel != "poolside/laguna-s-2.1" {
		t.Fatalf("wire model = %q, want %q", wireModel, "poolside/laguna-s-2.1")
	}
	var env map[string]any
	if err := json.Unmarshal(wireBody, &env); err != nil {
		t.Fatalf("wire body is not valid JSON: %v", err)
	}
	msgs, _ := env["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if _, has := first["reasoning"]; has {
		t.Fatalf("wire body still carries colliding reasoning key: %s", wireBody)
	}
	if _, has := first["reasoning_content"]; !has {
		t.Fatalf("wire body lost reasoning_content: %s", wireBody)
	}
}

// TestForwardPoolsideModelRewrite drives a bare ps/-resolved model id through
// the executor against a fake upstream and asserts the wire model is scoped
// while the response passes through unchanged.
func TestForwardPoolsideModelRewrite(t *testing.T) {
	var wireModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var env map[string]any
		_ = json.Unmarshal(raw, &env)
		wireModel, _ = env["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)

	rec := httptest.NewRecorder()
	err := ForwardPoolside(rec, &Request{
		Ctx:    t.Context(),
		Client: srv.Client(),
		Config: &providers.ProviderConfig{BaseURL: srv.URL + "/v1/chat/completions"},
		APIKey: "sky_test",
		Body:   []byte(`{"model":"laguna-s-2.1","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("ForwardPoolside() error = %v", err)
	}
	if wireModel != "poolside/laguna-s-2.1" {
		t.Fatalf("wire model = %q, want %q", wireModel, "poolside/laguna-s-2.1")
	}
	if body := rec.Body.String(); !strings.Contains(body, "pong") {
		t.Fatalf("response passthrough lost content: %s", body)
	}
}
