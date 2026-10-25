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

func cloudflareWireRoles(t *testing.T, body []byte) []string {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal wire body: %v", err)
	}
	msgs, _ := env["messages"].([]any)
	var roles []string
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		roles = append(roles, msg["role"].(string))
	}
	return roles
}

func TestMergeCloudflareSystemMessages(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantBody string // "" means: body must be returned unchanged
	}{
		{
			name:     "single leading system is untouched",
			in:       `{"model":"m","messages":[{"role":"system","content":"A"},{"role":"user","content":"hi"}]}`,
			wantBody: "",
		},
		{
			name:     "two leading systems merge in order",
			in:       `{"model":"m","messages":[{"role":"system","content":"A"},{"role":"system","content":"B"},{"role":"user","content":"hi"}]}`,
			wantBody: `{"model":"m","messages":[{"role":"system","content":"A\n\nB"},{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "system after user moves to front and merges",
			in:       `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":"A"},{"role":"assistant","content":"yo"},{"role":"system","content":"B"}]}`,
			wantBody: `{"model":"m","messages":[{"role":"system","content":"A\n\nB"},{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`,
		},
		{
			name:     "hermes shape: two systems then history",
			in:       `{"model":"m","messages":[{"role":"system","content":"A"},{"role":"system","content":"B"},{"role":"user","content":"Halo"},{"role":"assistant","content":"Halo!"},{"role":"user","content":"Halo"}]}`,
			wantBody: `{"model":"m","messages":[{"role":"system","content":"A\n\nB"},{"role":"user","content":"Halo"},{"role":"assistant","content":"Halo!"},{"role":"user","content":"Halo"}]}`,
		},
		{
			name:     "system block array merges text parts",
			in:       `{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"A "},{"type":"text","text":"B"}]},{"role":"system","content":"C"},{"role":"user","content":"hi"}]}`,
			wantBody: `{"model":"m","messages":[{"role":"system","content":"A B\n\nC"},{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "no system is untouched",
			in:       `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			wantBody: "",
		},
		{
			name:     "non-object body is untouched",
			in:       `not json`,
			wantBody: "",
		},
		{
			name:     "unmergeable system content is forwarded, not dropped",
			in:       `{"model":"m","messages":[{"role":"system","content":42},{"role":"system","content":"B"},{"role":"user","content":"hi"}]}`,
			wantBody: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := mergeCloudflareSystemMessages([]byte(tt.in))
			if tt.wantBody == "" {
				if changed || string(got) != tt.in {
					t.Fatalf("merge = changed=%v body=%s, want unchanged", changed, got)
				}
				return
			}
			if !changed {
				t.Fatalf("merge reported no change, want merged body")
			}
			// Compare semantically: key order in the re-marshalled body
			// is not part of the contract.
			var gotEnv, wantEnv map[string]any
			if err := json.Unmarshal(got, &gotEnv); err != nil {
				t.Fatalf("unmarshal merged body: %v", err)
			}
			if err := json.Unmarshal([]byte(tt.wantBody), &wantEnv); err != nil {
				t.Fatalf("unmarshal want body: %v", err)
			}
			gotJSON, _ := json.Marshal(gotEnv)
			wantJSON, _ := json.Marshal(wantEnv)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("merged = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestForwardCloudflareAI_MergesSystems drives a Hermes-shaped two-system
// payload through the executor against a fake upstream and asserts the
// wire carries exactly one leading system message while other fields
// (model, tools, the rest of the history) pass through unchanged.
func TestForwardCloudflareAI_MergesSystems(t *testing.T) {
	var wireBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wireBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)

	rec := httptest.NewRecorder()
	err := ForwardCloudflareAI(rec, &Request{
		Ctx:    t.Context(),
		Client: srv.Client(),
		Config: &providers.ProviderConfig{BaseURL: srv.URL + "/v1/chat/completions"},
		APIKey: "sky_test",
		Body:   []byte(`{"model":"@cf/qwen/qwen3.8-27b","messages":[{"role":"system","content":"A"},{"role":"system","content":"B"},{"role":"user","content":"Halo"}],"tools":[{"type":"function","function":{"name":"f"}}]}`),
	})
	if err != nil {
		t.Fatalf("ForwardCloudflareAI() error = %v", err)
	}
	if roles := cloudflareWireRoles(t, wireBody); len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
		t.Fatalf("wire roles = %v, want [system user]", roles)
	}
	var env map[string]any
	_ = json.Unmarshal(wireBody, &env)
	msgs := env["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "A\n\nB" {
		t.Fatalf("wire system content = %q, want %q", msgs[0].(map[string]any)["content"], "A\n\nB")
	}
	if _, ok := env["tools"]; !ok {
		t.Fatalf("wire body lost tools field: %s", wireBody)
	}
	if body := rec.Body.String(); !strings.Contains(body, "pong") {
		t.Fatalf("response passthrough lost content: %s", body)
	}
}
