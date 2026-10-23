package chat

import (
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"9router/proxy/internal/constants"
	"9router/proxy/internal/providers"
	internalproxy "9router/proxy/internal/proxy"
)

// tailOnlyCursorBody keeps system prompts, drops bulk history, and keeps the
// latest turns. Short sessions and unparsable bodies return nil.
func TestTailOnlyCursorBody(t *testing.T) {
	msgs := make([]any, 0, 90)
	for i := 0; i < 85; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("history %d", i)})
	}
	msgs = append(msgs,
		map[string]any{"role": "system", "content": "sys prompt"},
		map[string]any{"role": "user", "content": "latest question"},
		map[string]any{"role": "assistant", "content": "latest answer"},
	)
	body, _ := json.Marshal(map[string]any{"model": "cursor/default", "messages": msgs})

	tail := tailOnlyCursorBody(body)
	if tail == nil {
		t.Fatal("long session must produce a tail frame")
	}
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(tail, &out); err != nil {
		t.Fatalf("tail frame must parse: %v", err)
	}
	if len(out.Messages) >= len(msgs) {
		t.Fatalf("tail frame must be shorter: %d vs %d", len(out.Messages), len(msgs))
	}
	var kept, notice bool
	for _, m := range out.Messages {
		switch m["role"] {
		case "system":
			content, _ := m["content"].(string)
			switch {
			case content == "sys prompt":
				kept = true
			case strings.Contains(content, "[9router trim]"):
				notice = true
			}
		case "user":
			if content, _ := m["content"].(string); content == "history 0" {
				t.Error("bulk history must be dropped")
			}
		}
	}
	if !kept {
		t.Error("system prompt must survive the trim")
	}
	if !notice {
		t.Error("trim notice must be injected")
	}
	tail2 := tailOnlyCursorBody(tail)
	if tail2 != nil {
		var out2 struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(tail2, &out2)
		notices := 0
		for _, m := range out2.Messages {
			if content, _ := m["content"].(string); strings.Contains(content, "[9router trim]") {
				notices++
			}
		}
		if notices > 1 {
			t.Errorf("re-trim stacked %d notices", notices)
		}
	}
}

func TestTailOnlyCursorBodyShortSession(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model":    "cursor/default",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if tail := tailOnlyCursorBody(body); tail != nil {
		t.Error("short session must not produce a tail frame")
	}
	if tail := tailOnlyCursorBody([]byte("not json")); tail != nil {
		t.Error("unparsable body must not produce a tail frame")
	}
}

// Cursor Auto resolve: the picker alias reaches the resolver as a concrete id.
func TestResolveModelCursorAuto(t *testing.T) {
	h := NewChatHandler(nil)
	for _, model := range []string{"cursor/default", "cursor/auto"} {
		info, err := h.ResolveModel(model)
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if info.Provider != "cursor" {
			t.Fatalf("%s resolved to provider %q", model, info.Provider)
		}
		if info.Model == "default" || info.Model == "auto" {
			t.Errorf("%s forwarded literally: %q", model, info.Model)
		}
	}
	info, err := h.ResolveModel("cursor/claude-4.5-sonnet")
	if err != nil || info.Model != "claude-4.5-sonnet" {
		t.Errorf("concrete id must pass through, got %+v, %v", info, err)
	}
}

// Empty-turn recovery: a long session whose full replay returns a 200 with
// no completion is retried on a tail-only frame and recovers content.
func TestCursorEmptyTurnRecovery(t *testing.T) {
	t.Setenv("CURSOR_EMPTY_TURN_RETRY_DELAY_MS", "0")
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []any `json:"messages"`
		}
		_ = json.Unmarshal(raw, &req)
		calls.Add(1)
		if len(req.Messages) >= 80 {
			t.Errorf("retry must carry a tail-only frame, got %d messages", len(req.Messages))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	msgs := make([]any, 0, 85)
	for i := 0; i < 84; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("m%d", i)})
	}
	body, _ := json.Marshal(map[string]any{"model": "x", "messages": msgs})

	h, cleanup := setupHandlerForForward(t)
	defer cleanup()
	cfg := &providers.ProviderConfig{
		BaseURL:    upstream.URL,
		AuthScheme: constants.AuthSchemeBearer,
		AuthHeader: constants.HeaderAuthorization,
	}
	rec := httptest.NewRecorder()
	firstErr := internalproxy.UpstreamFailure(http.StatusBadGateway, "upstream answered 200 without a completion: the first choice carries no content, tool call or reasoning")
	if err := h.retryCursorEmptyTurn(t.Context(), rec, cfg, "k", body, false, false, &streamMetrics{}, nil, firstErr, "cursor"); err != nil {
		t.Fatalf("tail-only retry must recover: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one retry attempt, got %d", calls.Load())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("retry must serve a completion: %s", rec.Body.String())
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if content, _ := msg["content"].(string); content != "recovered" {
		t.Errorf("retry served %q, want recovered content", content)
	}
}

// Short sessions never retry: a first-message empty turn is a protocol
// condition, not a wedged replay, and surfaces untried.
func TestCursorEmptyTurnShortSessionNoRetry(t *testing.T) {
	h, cleanup := setupHandlerForForward(t)
	defer cleanup()
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	firstErr := internalproxy.UpstreamFailure(http.StatusBadGateway, "upstream answered 200 without a completion: the first choice carries no content, tool call or reasoning")
	rec := httptest.NewRecorder()
	cfg := &providers.ProviderConfig{BaseURL: "http://127.0.0.1:1"}
	if err := h.retryCursorEmptyTurn(t.Context(), rec, cfg, "k", body, false, false, &streamMetrics{}, nil, firstErr, "cursor"); err != firstErr {
		t.Errorf("short session must surface the original error untried, got %v", err)
	}
}
