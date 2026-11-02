package executor

import (
	"context"
	"encoding/json/jsontext"
	"os"
	"testing"
)

// Live diagnostic: runs one real guest turn through a plain http.Client
// (env proxy), bypassing the router's client. Guarded by QWEN_WEB_LIVE=1.
func TestQwenWebLiveTurnPlainClient(t *testing.T) {
	if os.Getenv("QWEN_WEB_LIVE") != "1" {
		t.Skip("live test disabled")
	}
	client := qwenWebHTTPClient()
	in := []qwenWebInputMsg{
		{Role: "user", Content: jsontext.Value(`"ping, jawab pong saja"`)},
	}
	if os.Getenv("QWEN_WEB_HISTORY") == "1" {
		in = []qwenWebInputMsg{
			{Role: "user", Content: jsontext.Value(`"Ingat: angka favorit saya 4217."`)},
			{Role: "assistant", Content: jsontext.Value(`"Baik, saya ingat angka favorit Anda adalah 4217."`)},
			{Role: "user", Content: jsontext.Value(`"Berapa angka favorit saya?"`)},
		}
	}
	if os.Getenv("QWEN_WEB_SYSTEM") == "1" {
		in = append([]qwenWebInputMsg{{Role: "system", Content: jsontext.Value(`"You are a helpful assistant. Follow the ponytail style: be concise, write minimal code, and prefer simple solutions. This is a long system prompt standing in for the token-saver injection."`)}}, in...)
	}
	model := "qwen3.7-plus"
	if m := os.Getenv("QWEN_WEB_MODEL"); m != "" {
		model = m
	}
	msgs := qwenWebBuildMessages(in, model, true)
	acc, err := qwenWebTurnOnce(context.Background(), client, model, msgs, qwenWebCredential{}, false, func(rd, ad string) error {
		t.Logf("delta r=%q a=%q", rd, ad)
		return nil
	})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	t.Logf("answer=%q reasoning_len=%d", acc.answer.String(), acc.reasoning.Len())
}
