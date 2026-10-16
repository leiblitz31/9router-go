package translator

import "testing"

// PromptCompletionFromJSON is wired into the usage window fold
// (rowPromptCompletion), so its shapes are pinned here: OpenAI keys win,
// Claude/Responses input_tokens/output_tokens are the fallback for
// embedding/Gemini rows, and explicit null is not a value.
func TestPromptCompletionFromJSON(t *testing.T) {
	tests := []struct {
		name           string
		raw            string
		wantPrompt     int
		wantCompletion int
	}{
		{"openai shape", `{"prompt_tokens":100,"completion_tokens":50}`, 100, 50},
		{"claude input/output fallback", `{"input_tokens":200,"output_tokens":60}`, 200, 60},
		{"openai wins over input", `{"prompt_tokens":10,"input_tokens":200,"completion_tokens":5,"output_tokens":60}`, 10, 5},
		{"missing keys yield zero", `{}`, 0, 0},
		{"empty blob", ``, 0, 0},
		{"explicit null is not a value", `{"prompt_tokens":null,"completion_tokens":null}`, 0, 0},
		{"null falls back to input", `{"prompt_tokens":null,"input_tokens":30}`, 30, 0},
		{"invalid json", `not json`, 0, 0},
		{"negative is not a value", `{"prompt_tokens":-1,"input_tokens":30}`, 30, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, c := PromptCompletionFromJSON([]byte(tt.raw))
			if p != tt.wantPrompt || c != tt.wantCompletion {
				t.Errorf("PromptCompletionFromJSON(%q) = (%d,%d), want (%d,%d)",
					tt.raw, p, c, tt.wantPrompt, tt.wantCompletion)
			}
		})
	}
}
