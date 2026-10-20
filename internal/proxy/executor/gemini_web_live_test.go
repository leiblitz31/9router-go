package executor

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"9router/proxy/internal/providers"
)

// A live smoke against the real gemini.google.com.
//
// The site is a third party, so the test is opt-in: it is skipped unless
// 9ROUTER_LIVE_TESTS=1 is set, and CI does not set it. The session is
// anonymous, so no credential is involved.
//
// Run it with:
//
//	9ROUTER_LIVE_TESTS=1 go test ./internal/proxy/executor/ -run TestGeminiWebLive -v
func TestGeminiWebLiveAnonymousTurn(t *testing.T) {
	if os.Getenv("9ROUTER_LIVE_TESTS") == "" {
		t.Skip("live upstream test: set 9ROUTER_LIVE_TESTS=1 to run it against gemini.google.com")
	}
	rec := httptest.NewRecorder()
	err := ForwardGeminiWeb(rec, &Request{
		Ctx:      context.Background(),
		Config:   &providers.ProviderConfig{},
		Body:     geminiWebTestBody(t, "gemini-3.1-flash-lite", "Reply with the word pong and nothing else.", nil),
		IsStream: true,
	})
	if err != nil {
		t.Fatalf("ForwardGeminiWeb() error = %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pong") {
		t.Errorf("the answer carries no pong:\n%s", body)
	}
	if !strings.Contains(body, "\"usage\"") {
		t.Errorf("the stream carries no usage frame:\n%s", body)
	}
}
