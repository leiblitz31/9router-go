package executor

import (
	"context"
	"encoding/json/jsontext"
	"net/http"
	"os"
	"testing"
	"time"
)

// Live diagnostic: runs one real guest turn through a plain http.Client
// (env proxy), bypassing the router's client. Guarded by QWEN_WEB_LIVE=1.
func TestQwenWebLiveTurnPlainClient(t *testing.T) {
	if os.Getenv("QWEN_WEB_LIVE") != "1" {
		t.Skip("live test disabled")
	}
	client := &http.Client{Timeout: 120 * time.Second}
	msgs := qwenWebBuildMessages([]qwenWebInputMsg{
		{Role: "user", Content: jsontext.Value(`"ping, jawab pong saja"`)},
	}, "qwen3.7-plus", true)
	acc, err := qwenWebTurnOnce(context.Background(), client, "qwen3.7-plus", msgs, qwenWebCredential{}, false, func(rd, ad string) error {
		t.Logf("delta r=%q a=%q", rd, ad)
		return nil
	})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	t.Logf("answer=%q reasoning_len=%d", acc.answer.String(), acc.reasoning.Len())
}
