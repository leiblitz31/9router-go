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
