package executor

import (
	"context"
	"strings"
	"testing"
)

func TestMimoWebParseCredentials(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    mimoWebCredentials
		wantErr bool
	}{
		{
			name:  "json object",
			input: `{"serviceToken":"tok123","userId":"42","xiaomichatbot_ph":"ph_abc"}`,
			want:  mimoWebCredentials{ServiceToken: "tok123", UserID: "42", PH: "ph_abc"},
		},
		{
			name:  "cookie header copied from devtools",
			input: "userId=42; serviceToken=tok123; xiaomichatbot_ph=\"ph_abc\"; other=zzz",
			want:  mimoWebCredentials{ServiceToken: "tok123", UserID: "42", PH: "ph_abc"},
		},
		{
			name:  "prefixed token name and cookie prefix",
			input: "Cookie: xiaomichatbot_serviceToken=\"tok123\"; userId=42; xiaomichatbot_ph=ph_abc",
			want:  mimoWebCredentials{ServiceToken: "tok123", UserID: "42", PH: "ph_abc"},
		},
		{
			name:    "missing ph is an error",
			input:   "serviceToken=tok123; userId=42",
			wantErr: true,
		},
		{
			name:    "empty is an error",
			input:   "   ",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mimoWebParseCredentials(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("mimoWebParseCredentials() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("mimoWebParseCredentials() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMimoWebTurnApply(t *testing.T) {
	stream := "event: dialogId\n" +
		"data: {\"dialogId\":\"dlg-1\"}\n\n" +
		"event: message\n" +
		"data: {\"reasoningContent\":\"think \",\"content\":\"\"}\n\n" +
		"event: message\n" +
		"data: {\"reasoningContent\":\"more\",\"content\":\"Hello \"}\n\n" +
		"event: message\n" +
		"data: {\"content\":\"world\"}\n\n" +
		"event: usage\n" +
		"data: {\"promptTokens\":10,\"completionTokens\":5,\"totalTokens\":15}\n\n" +
		"event: finish\n" +
		"data: {}\n\n"

	turn := &mimoWebTurn{}
	err := mimoWebScanEvents(context.Background(), strings.NewReader(stream), func(ev mimoWebEvent) bool {
		turn.apply(ev, nil)
		return true
	})
	if err != nil {
		t.Fatalf("mimoWebScanEvents() error = %v", err)
	}
	if got := turn.content.String(); got != "Hello world" {
		t.Errorf("content = %q, want %q", got, "Hello world")
	}
	if got := turn.reasoning.String(); got != "think more" {
		t.Errorf("reasoning = %q, want %q", got, "think more")
	}
	if turn.dialogID != "dlg-1" {
		t.Errorf("dialogID = %q, want %q", turn.dialogID, "dlg-1")
	}
	usage := turn.usageMap()
	if usage == nil || usage["total_tokens"] != 15 {
		t.Errorf("usage = %v, want total_tokens 15", usage)
	}
}

func TestMimoWebTurnErrorEvent(t *testing.T) {
	stream := "event: error\ndata: {\"message\":\"model name error\"}\n\n"
	turn := &mimoWebTurn{}
	err := mimoWebScanEvents(context.Background(), strings.NewReader(stream), func(ev mimoWebEvent) bool {
		turn.apply(ev, nil)
		return turn.errText == ""
	})
	if err != nil {
		t.Fatalf("mimoWebScanEvents() error = %v", err)
	}
	if turn.errText != "model name error" {
		t.Errorf("errText = %q, want %q", turn.errText, "model name error")
	}
}

func TestMimoWebFlattenMessages(t *testing.T) {
	tests := []struct {
		name     string
		messages []any
		want     string
	}{
		{
			name:     "single user message verbatim",
			messages: []any{map[string]any{"role": "user", "content": "halo"}},
			want:     "halo",
		},
		{
			name: "transcript labelled by role",
			messages: []any{
				map[string]any{"role": "system", "content": "be brief"},
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "content": "hello"},
				map[string]any{"role": "user", "content": "again"},
			},
			want: "System: be brief\n\nUser: hi\n\nAssistant: hello\n\nUser: again",
		},
		{
			name: "content parts joined",
			messages: []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "look"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/y.png"}},
				}},
			},
			want: "look\n[image attached]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mimoWebFlattenMessages(tt.messages); got != tt.want {
				t.Errorf("mimoWebFlattenMessages() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMimoWebChatURL(t *testing.T) {
	client := newMimoWebClient(nil, mimoWebCredentials{ServiceToken: "t", UserID: "1", PH: "p h"})
	if got := client.chatURL("mimo-v2.6-flash"); !strings.HasPrefix(got, mimoWebBaseURL+mimoWebChatPath+"?xiaomichatbot_ph=p+h") {
		t.Errorf("chatURL(flash) = %q", got)
	}
	if got := client.chatURL(mimoWebUltraModel); !strings.HasPrefix(got, mimoWebBaseURL+mimoWebUltraChatPath+"?") {
		t.Errorf("chatURL(ultra) = %q", got)
	}
}
