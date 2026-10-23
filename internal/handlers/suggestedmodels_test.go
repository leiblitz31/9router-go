package handlers

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestSuggestedModelsCommandCodeFree covers the Command Code free-tier filter.
// The feed is the real /provider/v1/models shape: a flat `data` array whose
// entries carry `context_length` but no pricing block, so the ":free"/"-free"
// id suffix is the only signal. beam:free is the one the operator noticed
// missing; the paid minimax rows prove the filter is not a passthrough.
func TestSuggestedModelsCommandCodeFree(t *testing.T) {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[
			{"id":"reflection/beam:free","name":"Beam 1.0","context_length":1048576,"supported_endpoints":["/chat/completions"]},
			{"id":"stealth/glyph-cluster:free","name":"Glyph Cluster","context_length":262144},
			{"id":"poolside/laguna-s-2.1-free","name":"Laguna S 2.1","context_length":262144},
			{"id":"minimax/minimax-m3.1-flash","name":"MiniMax M3.1 Flash","context_length":1000000},
			{"id":"MiniMaxAI/MiniMax-M3","name":"MiniMax M3","context_length":1000000},
			{"id":"claude-sonnet-5-5","name":"Claude Sonnet 5.5","context_length":1000000}
		]}`))
	}))
	defer feed.Close()

	req := httptest.NewRequest(http.MethodGet,
		"/api/providers/suggested-models?url="+url.QueryEscape(feed.URL)+"&type=commandcode-free", nil)
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
	want := []string{"poolside/laguna-s-2.1-free", "reflection/beam:free", "stealth/glyph-cluster:free"}
	got := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		got = append(got, m.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
	// Sorted by id, so the second row carries beam's context window.
	if out.Data[1].ContextLength == nil || *out.Data[1].ContextLength != 1048576 {
		t.Fatalf("expected beam contextLength 1048576, got %v", out.Data[1].ContextLength)
	}
}

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
