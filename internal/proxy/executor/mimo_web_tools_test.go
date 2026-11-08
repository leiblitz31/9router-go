package executor

import (
	"strings"
	"testing"
)

func mimoToolsBody() map[string]any {
	return map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "read",
					"description": "read a file",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{"path": map[string]any{"type": "string"}},
						"required":   []any{"path"},
					},
				},
			},
		},
	}
}

func TestMimoWebRenderToolsTeachesDSML(t *testing.T) {
	tools := mimoWebParseTools(mimoToolsBody())
	got := mimoWebRenderTools(tools)
	for _, want := range []string{
		`"name": "read"`,
		"<｜｜DSML｜｜ calls>",
		"<｜｜DSML｜｜ invoke name=\"<tool name>\">",
		"never describe a call as attempted or failed",
		"[called <name> with <arguments>]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mimoWebRenderTools() missing %q", want)
		}
	}
}

func TestMimoWebParseTools(t *testing.T) {
	tools := mimoWebParseTools(mimoToolsBody())
	if len(tools) != 1 || tools[0].Function.Name != "read" {
		t.Fatalf("mimoWebParseTools() = %+v, want one read tool", tools)
	}
	if got := mimoWebParseTools(map[string]any{}); got != nil {
		t.Errorf("no tools key = %v, want nil", got)
	}
	none := mimoToolsBody()
	none["tool_choice"] = "none"
	if got := mimoWebParseTools(none); got != nil {
		t.Errorf("tool_choice none = %v, want nil", got)
	}
	flat := map[string]any{"tools": []any{
		map[string]any{"name": "bash", "description": "run", "input_schema": map[string]any{"type": "object"}},
	}}
	if got := mimoWebParseTools(flat); len(got) != 1 || got[0].Function.Name != "bash" {
		t.Errorf("flat spelling = %+v, want one bash tool", got)
	}
}

func mimoTurn(content, reasoning string) *mimoWebTurn {
	turn := &mimoWebTurn{}
	turn.content.WriteString(content)
	turn.reasoning.WriteString(reasoning)
	return turn
}

func TestMimoWebFinalizeDSMLCall(t *testing.T) {
	tools := mimoWebParseTools(mimoToolsBody())
	answer := "Let me check that file.\n" +
		"<｜｜DSML｜｜ calls>\n" +
		"<｜｜DSML｜｜ invoke name=\"read\">\n" +
		"<｜｜DSML｜｜ parameter name=\"path\" string=\"true\">/tmp/x.txt</｜｜DSML｜｜ parameter>\n" +
		"</｜｜DSML｜｜ invoke>\n" +
		"</｜｜DSML｜｜ calls>"
	message, finish := mimoWebFinalizeMessage(mimoTurn(answer, "thinking"), tools)
	if finish != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", finish)
	}
	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want one call", message["tool_calls"])
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if fn["name"] != "read" || !strings.Contains(fn["arguments"].(string), "/tmp/x.txt") {
		t.Errorf("call = %v", fn)
	}
	if content, _ := message["content"].(string); !strings.Contains(content, "Let me check") || strings.Contains(content, "DSML") {
		t.Errorf("content = %q, want the prose without markup", content)
	}
	if message["reasoning_content"] != "thinking" {
		t.Errorf("reasoning_content = %v", message["reasoning_content"])
	}
}

func TestMimoWebFinalizeJSONCall(t *testing.T) {
	tools := mimoWebParseTools(mimoToolsBody())
	message, finish := mimoWebFinalizeMessage(mimoTurn(`{"name": "read", "arguments": {"path": "/a/b"}}`, ""), tools)
	if finish != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", finish)
	}
	calls := message["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read" {
		t.Errorf("call = %v", fn)
	}
}

func TestMimoWebFinalizePlainAnswerStripsMarkup(t *testing.T) {
	tools := mimoWebParseTools(mimoToolsBody())
	// A DSML block naming an undeclared tool is markup, not an answer.
	answer := "Here is the summary.\n<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"undeclared\">\n</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"
	message, finish := mimoWebFinalizeMessage(mimoTurn(answer, ""), tools)
	if finish != "stop" {
		t.Fatalf("finish = %q, want stop", finish)
	}
	content, _ := message["content"].(string)
	if strings.Contains(content, "DSML") || !strings.Contains(content, "summary") {
		t.Errorf("content = %q, want prose with markup stripped", content)
	}
	if _, ok := message["tool_calls"]; ok {
		t.Errorf("tool_calls present on a plain answer")
	}
}

func TestMimoWebAssistantTextRendersCalls(t *testing.T) {
	msg := map[string]any{
		"role":    "assistant",
		"content": "Checking now.",
		"tool_calls": []any{
			map[string]any{
				"id":   "call_read_1",
				"type": "function",
				"function": map[string]any{
					"name":      "read",
					"arguments": `{"path": "/tmp/x.txt"}`,
				},
			},
		},
	}
	got := mimoWebAssistantText(msg)
	if !strings.Contains(got, "Checking now.") || !strings.Contains(got, "[called read with {\"path\": \"/tmp/x.txt\"}]") {
		t.Errorf("mimoWebAssistantText() = %q", got)
	}
}

func TestMimoWebAssistantTextSummarisesMarkupEcho(t *testing.T) {
	msg := map[string]any{
		"role": "assistant",
		"content": "<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"read\">\n" +
			"<｜｜DSML｜｜ parameter name=\"path\" string=\"true\">/tmp/x.txt</｜｜DSML｜｜ parameter>\n" +
			"</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>",
	}
	if got := mimoWebAssistantText(msg); got != "[called read]" {
		t.Errorf("mimoWebAssistantText() = %q, want [called read]", got)
	}
	plain := map[string]any{"role": "assistant", "content": "just prose"}
	if got := mimoWebAssistantText(plain); got != "just prose" {
		t.Errorf("plain assistant text changed: %q", got)
	}
}

func TestMimoWebFlattenWithToolsShowsCalls(t *testing.T) {
	tools := mimoWebParseTools(mimoToolsBody())
	messages := []any{
		map[string]any{"role": "user", "content": "read it"},
		map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{
				map[string]any{
					"id":   "call_read_1",
					"type": "function",
					"function": map[string]any{
						"name":      "read",
						"arguments": `{"path": "/tmp/x.txt"}`,
					},
				},
			},
		},
		map[string]any{"role": "tool", "content": "file body", "tool_call_id": "call_read_1"},
		map[string]any{"role": "user", "content": "thanks"},
	}
	got := mimoWebFlattenMessages(messages, tools)
	for _, want := range []string{"User: read it", "Assistant: [called read with {\"path\": \"/tmp/x.txt\"}]", "Tool result: file body", "User: thanks"} {
		if !strings.Contains(got, want) {
			t.Errorf("flatten missing %q in:\n%s", want, got)
		}
	}
}
