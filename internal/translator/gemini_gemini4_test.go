package translator

import (
	json "encoding/json/v2"
	"strings"
	"testing"
)

// TestIsGemini4Model is the Go port of upstream's isGemini4Model cases
// (open-sse/providers/capabilities.js, #4802).
func TestIsGemini4Model(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gemini-4-argon", true},
		{"gemini-4-argon-high", true},
		{"gemini-4.1-pro", true},
		{"google/gemini-4-argon", true},
		{"GEMINI-4-ARGON", true},
		{"gemini-3.8-flash", false},
		{"gemini-3-flash-preview", false},
		{"gemini-40-flash", false},
		{"gemini-4", true},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsGemini4Model(tc.model); got != tc.want {
			t.Errorf("IsGemini4Model(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// TestTranslateOpenAIToGemini_Gemini4DropsSamplingParams ports #4802: Gemini 4
// removed temperature/topP/topK, so forwarding them fails the request.
func TestTranslateOpenAIToGemini_Gemini4DropsSamplingParams(t *testing.T) {
	body := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_p":0.9,"top_k":40,"max_tokens":100}`)
	out, err := TranslateOpenAIToGemini(body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var req GeminiRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(req.GenerationConfig) == 0 {
		t.Fatalf("expected generationConfig with maxOutputTokens, got none: %s", out)
	}
	var gen map[string]any
	if err := json.Unmarshal(req.GenerationConfig, &gen); err != nil {
		t.Fatalf("unmarshal generationConfig: %v", err)
	}
	for _, k := range []string{"temperature", "topP", "topK"} {
		if _, ok := gen[k]; ok {
			t.Errorf("gemini-4 request must not carry %q: %s", k, out)
		}
	}
	if gen["maxOutputTokens"] != float64(100) {
		t.Errorf("maxOutputTokens = %v, want 100: %s", gen["maxOutputTokens"], out)
	}
}

// TestTranslateOpenAIToGemini_Gemini3KeepsSamplingParams is the regression
// guard: non-Gemini-4 models still forward the sampling params.
func TestTranslateOpenAIToGemini_Gemini3KeepsSamplingParams(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_p":0.9,"top_k":40}`)
	out, err := TranslateOpenAIToGemini(body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(string(out), `"temperature":0.7`) {
		t.Errorf("gemini-3 request must keep temperature: %s", out)
	}
	if !strings.Contains(string(out), `"topP":0.9`) {
		t.Errorf("gemini-3 request must keep topP: %s", out)
	}
	if !strings.Contains(string(out), `"topK":40`) {
		t.Errorf("gemini-3 request must keep topK: %s", out)
	}
}

// TestNormalizeGeminiContents_Gemini4FunctionResponseLast ports #4802: Gemini
// 4 rejects a merged user turn whose last part is not a functionResponse.
func TestNormalizeGeminiContents_Gemini4FunctionResponseLast(t *testing.T) {
	fr := func(name string) GeminiPart {
		return GeminiPart{FunctionResponse: &GeminiFunctionResp{Name: name}}
	}
	contents := []GeminiContent{
		{Role: "user", Parts: []GeminiPart{fr("tool_a")}},
		{Role: "user", Parts: []GeminiPart{{Text: "note"}}},
	}
	got := NormalizeGeminiContents(contents, "gemini-4-argon")
	if len(got) != 1 {
		t.Fatalf("merged turns = %d, want 1: %+v", len(got), got)
	}
	parts := got[0].Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %+v, want 2", parts)
	}
	if parts[0].FunctionResponse != nil || parts[1].FunctionResponse == nil {
		t.Errorf("functionResponse must sort last: %+v", parts)
	}
}

// TestNormalizeGeminiContents_Gemini3MergeOrderUnchanged is the regression
// guard: without a Gemini 4 model the merge keeps document order.
func TestNormalizeGeminiContents_Gemini3MergeOrderUnchanged(t *testing.T) {
	fr := GeminiPart{FunctionResponse: &GeminiFunctionResp{Name: "tool_a"}}
	contents := []GeminiContent{
		{Role: "user", Parts: []GeminiPart{fr}},
		{Role: "user", Parts: []GeminiPart{{Text: "note"}}},
	}
	for _, model := range []string{"", "gemini-3.8-flash"} {
		got := NormalizeGeminiContents(contents, model)
		if len(got) != 1 || len(got[0].Parts) != 2 {
			t.Fatalf("model %q: %+v", model, got)
		}
		if got[0].Parts[0].FunctionResponse == nil || got[0].Parts[1].Text != "note" {
			t.Errorf("model %q must keep document order: %+v", model, got[0].Parts)
		}
	}
}
