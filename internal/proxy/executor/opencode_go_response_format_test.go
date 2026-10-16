package executor

import (
	"9router/proxy/internal/providers"
	json "encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// Ports upstream 468aa7d9: OpenCode Go's relay 400s response_format (both
// json_schema and json_object — drop, not downgrade) for its deepseek family
// while serving it for every other model on the same relay, so the go lane
// drops the field for deepseek ids only.
func TestForwardOpencodeGo_DeepseekDropsResponseFormat(t *testing.T) {
	const schema = `{"type":"object","properties":{"guests":{"type":"integer"}}}`
	jsonSchemaRF := `"response_format":{"type":"json_schema","json_schema":{"name":"reading","schema":` + schema + `}}`
	jsonObjectRF := `"response_format":{"type":"json_object"}`
	tests := []struct {
		name        string
		model       string
		rf          string
		wantDropped bool
	}{
		{"deepseek json_schema dropped", "deepseek-v4.1-flash", jsonSchemaRF, true},
		{"deepseek json_object dropped", "deepseek-v4.1-flash", jsonObjectRF, true},
		{"deepseek prefix match is case-insensitive", "DeepSeek-V4", jsonSchemaRF, true},
		{"glm keeps json_schema", "glm-5.2", jsonSchemaRF, false},
		{"glm keeps json_object", "glm-5.2", jsonObjectRF, false},
		{"kimi keeps json_schema", "kimi-k2", jsonSchemaRF, false},
		{"mimo keeps json_schema", "mimo-v2-flash", jsonSchemaRF, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Errorf("unmarshal upstream body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}]}`))
			}))
			defer srv.Close()
			rec := httptest.NewRecorder()
			req := &Request{
				Client:        srv.Client(),
				Config:        &providers.ProviderConfig{BaseURL: srv.URL},
				APIKey:        "secret-go-key",
				Body:          []byte(`{"model":` + strconv.Quote(tt.model) + `,"messages":[{"role":"user","content":"hi"}],` + tt.rf + `}`),
				IsStream:      false,
				TranslateResp: false,
			}
			if err := ForwardOpencodeGo(rec, req); err != nil {
				t.Fatalf("ForwardOpencodeGo: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			_, present := got["response_format"]
			if present == tt.wantDropped {
				t.Errorf("model %q: response_format present=%v, want dropped=%v", tt.model, present, tt.wantDropped)
			}
		})
	}
}
