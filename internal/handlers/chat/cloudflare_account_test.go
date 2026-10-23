package chat

import (
	"os"
	"strings"
	"testing"

	"9router/proxy/internal/db"
)

// Cloudflare Workers AI names the account in the URL path, and the account id
// is per-connection state stored in providerSpecificData. The registry URL
// therefore carries an {accountId} placeholder; getProviderConfig must fill
// it from the selected connection. Before this, the registry baked in the
// CLOUDFLARE_ACCOUNT_ID env var — empty on any multi-account deployment — and
// every cf/* chat request went to /accounts//ai/v1/... (Cloudflare 7003)
// while the dashboard connection test, which does substitute, stayed green.
func TestGetProviderConfig_CloudflareAccountIDFromConnection(t *testing.T) {
	if os.Getenv("CLOUDFLARE_ACCOUNT_ID") != "" {
		t.Skip("registry URL is env-baked when CLOUDFLARE_ACCOUNT_ID is set")
	}
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	handler := NewChatHandler(db.NewRepo(database))

	cfg, err := handler.getProviderConfig("cloudflare-ai", &ConnectionData{
		ProviderSpecificData: map[string]any{"accountId": "acc123"},
	})
	if err != nil {
		t.Fatalf("getProviderConfig: %v", err)
	}
	want := "https://api.cloudflare.com/client/v4/accounts/acc123/ai/v1/chat/completions"
	if cfg.BaseURL != want {
		t.Errorf("resolved BaseURL = %q, want %q", cfg.BaseURL, want)
	}
}

// End to end through connection selection: the account id stored in the
// connection row's data JSON must reach the resolved URL.
func TestGetProviderConfig_CloudflareAccountIDSeededConnection(t *testing.T) {
	if os.Getenv("CLOUDFLARE_ACCOUNT_ID") != "" {
		t.Skip("registry URL is env-baked when CLOUDFLARE_ACCOUNT_ID is set")
	}
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	if _, err := database.Exec(`INSERT INTO providerConnections
		(id, provider, authType, name, priority, isActive, data, createdAt, updatedAt)
		VALUES ('conn-cf', 'cloudflare-ai', 'apikey', 'CF', 1, 1, ?, '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`,
		`{"apiKey":"cf-token","providerSpecificData":{"accountId":"f4d070a913f402fd68306d4c95b4c60a"}}`); err != nil {
		t.Fatalf("seed connection: %v", err)
	}

	handler := NewChatHandler(db.NewRepo(database))
	_, connData, err := handler.getBestConnection("cloudflare-ai", "conn-cf", nil, "")
	if err != nil {
		t.Fatalf("getBestConnection: %v", err)
	}

	cfg, err := handler.getProviderConfig("cloudflare-ai", connData)
	if err != nil {
		t.Fatalf("getProviderConfig: %v", err)
	}
	if !strings.Contains(cfg.BaseURL, "/accounts/f4d070a913f402fd68306d4c95b4c60a/ai/v1/chat/completions") {
		t.Errorf("resolved BaseURL = %q, want the connection's account id in the path", cfg.BaseURL)
	}
}

// A connection without an account id leaves the placeholder untouched — the
// request still fails upstream, but the resolver must not invent an account
// or corrupt the URL.
func TestGetProviderConfig_CloudflareNoAccountIDLeavesPlaceholder(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	handler := NewChatHandler(db.NewRepo(database))

	cfg, err := handler.getProviderConfig("cloudflare-ai", &ConnectionData{})
	if err != nil {
		t.Fatalf("getProviderConfig: %v", err)
	}
	if strings.Contains(cfg.BaseURL, "/accounts//") {
		t.Errorf("resolved BaseURL = %q, must never contain an empty account segment", cfg.BaseURL)
	}
}
