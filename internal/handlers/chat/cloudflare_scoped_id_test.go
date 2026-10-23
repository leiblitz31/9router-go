package chat

import (
	"testing"

	"9router/proxy/internal/db"
)

// Cloudflare Workers AI ids are scoped — "@cf/<vendor>/<model>" is the whole
// upstream id. A client that copies the id from Cloudflare (Hermes' model
// picker does) sends it without the router's "cf/" prefix, and resolution
// used to read the provider as "@cf", which has no connections, so every
// such request failed 502 "no active connections for provider: @cf" while
// the prefixed form worked. The scoped form must resolve to cloudflare-ai
// with the model id — scope included — untouched.
func TestResolveModel_CloudflareScopedID(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	handler := NewChatHandler(db.NewRepo(database))

	tests := []struct {
		name         string
		input        string
		wantProvider string
		wantModel    string
	}{
		{
			name:         "scoped id from a client model pick",
			input:        "@cf/moonshotai/kimi-k2.7-code",
			wantProvider: "cloudflare-ai",
			wantModel:    "@cf/moonshotai/kimi-k2.7-code",
		},
		{
			name:         "scoped id keeps its scope",
			input:        "@cf/qwen/qwen3.8-27b",
			wantProvider: "cloudflare-ai",
			wantModel:    "@cf/qwen/qwen3.8-27b",
		},
		{
			name:         "prefixed form unchanged",
			input:        "cf/@cf/qwen/qwen3.8-27b",
			wantProvider: "cloudflare-ai",
			wantModel:    "@cf/qwen/qwen3.8-27b",
		},
		{
			name:         "canonical provider form unchanged",
			input:        "cloudflare-ai/@cf/meta/llama-3.3-70b-instruct-fp8-fast",
			wantProvider: "cloudflare-ai",
			wantModel:    "@cf/meta/llama-3.3-70b-instruct-fp8-fast",
		},
		{
			name:         "unrelated provider unchanged",
			input:        "deepseek/deepseek-chat",
			wantProvider: "deepseek",
			wantModel:    "deepseek-chat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := handler.resolveModel(tt.input)
			if err != nil {
				t.Fatalf("resolveModel(%q): %v", tt.input, err)
			}
			if info.Provider != tt.wantProvider {
				t.Errorf("resolveModel(%q).Provider = %q, want %q", tt.input, info.Provider, tt.wantProvider)
			}
			if info.Model != tt.wantModel {
				t.Errorf("resolveModel(%q).Model = %q, want %q", tt.input, info.Model, tt.wantModel)
			}
		})
	}
}
