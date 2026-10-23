package handlers

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestSuggestedModelsOpencodeFree(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[
			{"id":"muse-spark-free"},
			{"id":"big-pickle"},
			{"id":"deepseek-v4-flash-free"},
			{"id":"gpt-paid"}
		]}`))
	}))
	defer feed.Close()

	req := httptest.NewRequest(http.MethodGet,
		"/api/providers/suggested-models?url="+url.QueryEscape(feed.URL)+"&type=opencode-free", nil)
	rec := httptest.NewRecorder()
	HandleSuggestedModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []suggestedModel `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range out.Data {
		ids[m.ID] = true
	}
	if !ids["muse-spark-free"] || !ids["big-pickle"] {
		t.Fatalf("expected free models, got %v", out.Data)
	}
	if ids["deepseek-v4-flash-free"] || ids["gpt-paid"] {
		t.Fatalf("dead/paid models should be filtered, got %v", out.Data)
	}
}

func TestSuggestedModelsOpenRouterFree(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[
			{"id":"a-free","name":"A","pricing":{"prompt":"0","completion":"0"},"context_length":250000},
			{"id":"b-paid","name":"B","pricing":{"prompt":"0.01","completion":"0"},"context_length":300000},
			{"id":"c-short","name":"C","pricing":{"prompt":"0","completion":"0"},"context_length":32000}
		]}`))
	}))
	defer feed.Close()

	req := httptest.NewRequest(http.MethodGet,
		"/api/providers/suggested-models?url="+url.QueryEscape(feed.URL)+"&type=openrouter-free", nil)
	rec := httptest.NewRecorder()
	HandleSuggestedModels(rec, req)
	var out struct {
		Data []suggestedModel `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "a-free" {
		t.Fatalf("expected only a-free, got %v", out.Data)
	}
	if out.Data[0].ContextLength == nil || *out.Data[0].ContextLength != 250000 {
		t.Fatalf("expected contextLength 250000, got %v", out.Data[0])
	}
}

func TestSuggestedModelsBadRequests(t *testing.T) {
	for _, target := range []string{
		"/api/providers/suggested-models",
		"/api/providers/suggested-models?url=http://x&type=nope",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		HandleSuggestedModels(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d", target, rec.Code)
		}
	}

	// Unreachable feed → 200 with empty data (upstream parity).
	req := httptest.NewRequest(http.MethodGet,
		"/api/providers/suggested-models?url=http://127.0.0.1:1/nope&type=mimo-free", nil)
	rec := httptest.NewRecorder()
	HandleSuggestedModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unreachable feed: expected 200, got %d", rec.Code)
	}
	var out struct {
		Data []suggestedModel `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if out.Data == nil || len(out.Data) != 0 {
		t.Fatalf("expected empty data, got %v", out.Data)
	}
}

func TestSuggestedModelsNousFree(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[
			{"id":"vendor/free-chat","name":"Free Chat","context_length":262144,"pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}},
			{"id":"vendor/paid-chat","context_length":262144,"pricing":{"prompt":"0.01","completion":"0"},"architecture":{"output_modalities":["text"]}},
			{"id":"vendor/short-chat","context_length":131072,"pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}},
			{"id":"vendor/embedding","context_length":262144,"pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["embeddings"]}}
		]}`))
	}))
	defer feed.Close()

	req := httptest.NewRequest(http.MethodGet,
		"/api/providers/suggested-models?url="+url.QueryEscape(feed.URL)+"&type=nous-free", nil)
	rec := httptest.NewRecorder()
	HandleSuggestedModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []suggestedModel `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "vendor/free-chat" {
		t.Fatalf("expected only vendor/free-chat, got %v", out.Data)
	}
	if out.Data[0].ContextLength == nil || *out.Data[0].ContextLength != 262144 {
		t.Fatalf("expected contextLength 262144, got %v", out.Data[0])
	}
}
