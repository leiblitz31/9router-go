package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnsureOpenCodeEnvMarkerText(t *testing.T) {
	complete := "Do x.\n\n<env>\n  Working directory: /workspace\n  Workspace root folder: /workspace\n</env>"
	tests := []struct {
		name  string
		in    string
		check func(t *testing.T, got string)
	}{
		{
			name: "empty becomes just the block",
			in:   "",
			check: func(t *testing.T, got string) {
				if got != openCodeEnvBlock {
					t.Errorf("got %q, want exactly the block", got)
				}
			},
		},
		{
			name: "no block is appended once",
			in:   "You are helpful.",
			check: func(t *testing.T, got string) {
				if !strings.HasPrefix(got, "You are helpful.") {
					t.Errorf("original text lost: %q", got)
				}
				if strings.Count(got, "<env>") != 1 {
					t.Errorf("expected exactly one <env> block, got %q", got)
				}
				if !strings.Contains(got, "Working directory: /workspace") || !strings.Contains(got, "Workspace root folder: /workspace") {
					t.Errorf("block incomplete: %q", got)
				}
			},
		},
		{
			name: "complete block is untouched",
			in:   complete,
			check: func(t *testing.T, got string) {
				if got != complete {
					t.Errorf("complete block changed:\n%q", got)
				}
			},
		},
		{
			name: "partial block is completed in place",
			in:   "Do x.\n\n<env>\n  Working directory: /workspace\n</env>",
			check: func(t *testing.T, got string) {
				if strings.Count(got, "<env>") != 1 {
					t.Errorf("second block added: %q", got)
				}
				if !strings.Contains(got, "Workspace root folder: /workspace") {
					t.Errorf("missing line not added: %q", got)
				}
			},
		},
		{
			name: "unclosed partial block is completed and closed",
			in:   "Do x.\n\n<env>\n  Working directory: /workspace",
			check: func(t *testing.T, got string) {
				if strings.Count(got, "<env>") != 1 || !strings.Contains(got, "</env>") {
					t.Errorf("block not closed: %q", got)
				}
				if !strings.Contains(got, "Workspace root folder: /workspace") {
					t.Errorf("missing line not added: %q", got)
				}
			},
		},
		{
			name: "marker lines outside an env block do not count",
			in:   "Working directory: /tmp\nDo x.",
			check: func(t *testing.T, got string) {
				if strings.Count(got, "<env>") != 1 {
					t.Errorf("expected one appended block, got %q", got)
				}
				if !strings.Contains(got, "Workspace root folder: /workspace") {
					t.Errorf("block incomplete: %q", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, ensureOpenCodeEnvMarkerText(tt.in))
		})
	}
}

func mustUnmarshalMap(t *testing.T, js string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatalf("bad test fixture %s: %v", js, err)
	}
	return m
}

func firstMessage(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("no messages in %v", m)
	}
	mm, ok := msgs[0].(map[string]any)
	if !ok {
		t.Fatalf("first message is not an object: %v", msgs[0])
	}
	return mm
}

func TestEnsureOpenCodeEnvMarker(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		check func(t *testing.T, m map[string]any)
	}{
		{
			name: "messages without system get a prepended system message",
			in:   `{"model":"x","messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, m map[string]any) {
				sys := firstMessage(t, m)
				if sys["role"] != "system" || sys["content"] != openCodeEnvBlock {
					t.Errorf("first message = %v, want system carrying only the block", sys)
				}
				msgs := m["messages"].([]any)
				if len(msgs) != 2 || msgs[1].(map[string]any)["role"] != "user" {
					t.Errorf("original messages not preserved: %v", msgs)
				}
			},
		},
		{
			name: "system string gains the block",
			in:   `{"messages":[{"role":"system","content":"Be nice."},{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, m map[string]any) {
				c, _ := firstMessage(t, m)["content"].(string)
				if !strings.HasPrefix(c, "Be nice.") || !strings.Contains(c, "<env>") {
					t.Errorf("system content = %q", c)
				}
			},
		},
		{
			name: "system string with complete block is untouched",
			in:   `{"messages":[{"role":"system","content":"Be nice.\n\n<env>\n  Working directory: /workspace\n  Workspace root folder: /workspace\n</env>"}]}`,
			check: func(t *testing.T, m map[string]any) {
				c, _ := firstMessage(t, m)["content"].(string)
				if strings.Count(c, "<env>") != 1 {
					t.Errorf("block duplicated: %q", c)
				}
			},
		},
		{
			name: "system content parts gain the block in the text part",
			in:   `{"messages":[{"role":"system","content":[{"type":"text","text":"Be nice."}]},{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, m map[string]any) {
				parts, _ := firstMessage(t, m)["content"].([]any)
				if len(parts) != 1 {
					t.Fatalf("parts = %v", parts)
				}
				txt, _ := parts[0].(map[string]any)["text"].(string)
				if !strings.Contains(txt, "<env>") {
					t.Errorf("text part = %q", txt)
				}
			},
		},
		{
			name: "system content parts without text gain a text part",
			in:   `{"messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`,
			check: func(t *testing.T, m map[string]any) {
				parts, _ := firstMessage(t, m)["content"].([]any)
				if len(parts) != 2 {
					t.Fatalf("parts = %v", parts)
				}
				txt, _ := parts[1].(map[string]any)["text"].(string)
				if txt != openCodeEnvBlock {
					t.Errorf("appended part text = %q", txt)
				}
			},
		},
		{
			name: "top-level system string gains the block",
			in:   `{"system":"Be nice.","messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, m map[string]any) {
				s, _ := m["system"].(string)
				if !strings.HasPrefix(s, "Be nice.") || !strings.Contains(s, "<env>") {
					t.Errorf("system = %q", s)
				}
			},
		},
		{
			name: "instructions string gains the block",
			in:   `{"instructions":"Be nice.","input":[]}`,
			check: func(t *testing.T, m map[string]any) {
				s, _ := m["instructions"].(string)
				if !strings.HasPrefix(s, "Be nice.") || !strings.Contains(s, "<env>") {
					t.Errorf("instructions = %q", s)
				}
			},
		},
		{
			name: "responses input without instructions gains instructions",
			in:   `{"input":[{"type":"message","role":"user","content":[]}]}`,
			check: func(t *testing.T, m map[string]any) {
				if m["instructions"] != openCodeEnvBlock {
					t.Errorf("instructions = %v", m["instructions"])
				}
			},
		},
		{
			name: "body with no system prompt shape is untouched",
			in:   `{"model":"x"}`,
			check: func(t *testing.T, m map[string]any) {
				if _, ok := m["messages"]; ok {
					t.Error("messages added to a body without any")
				}
				if _, ok := m["instructions"]; ok {
					t.Error("instructions added to a body without input")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mustUnmarshalMap(t, tt.in)
			ensureOpenCodeEnvMarker(m)
			tt.check(t, m)
			// Idempotency: a second pass must not change anything.
			before, _ := json.Marshal(m)
			ensureOpenCodeEnvMarker(m)
			after, _ := json.Marshal(m)
			if string(before) != string(after) {
				t.Errorf("second pass changed the body:\n%s\n->\n%s", before, after)
			}
		})
	}
}
