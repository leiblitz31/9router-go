package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

// Upstream (commandcode/cmc) rejects OpenAI-shaped tool definitions and
// OpenAI-shaped tool history: 400 "expected string, received undefined at
// params.tools[0].name". buildCommandcodeBody must translate both.
func TestBuildCommandcodeBody_ToolsShape(t *testing.T) {
	body := []byte(`{
		"model":"deepseek/deepseek-v4.1-flash",
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"weather in Jakarta?"},
			{"role":"assistant","content":"checking","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Jakarta\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"31C sunny"}
		],
		"tools":[{"type":"function","function":{
			"name":"get_weather","description":"weather",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}
		}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}}
	}`)

	out, err := buildCommandcodeBody(body, "deepseek/deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := payload["threadId"].(string); !ok {
		t.Error("missing threadId")
	}

	params := payload["params"].(map[string]any)

	tools := params["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Errorf("want name=get_weather, got %v", tool["name"])
	}
	if tool["description"] != "weather" {
		t.Errorf("want description, got %v", tool["description"])
	}
	if _, ok := tool["input_schema"].(map[string]any); !ok {
		t.Errorf("want input_schema object, got %v", tool["input_schema"])
	}
	if _, ok := tool["function"]; ok {
		t.Error("OpenAI 'function' wrapper must not survive")
	}

	tc := params["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "get_weather" {
		t.Errorf("want Anthropic tool_choice, got %v", tc)
	}

	msgs := params["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (system folded into params.system), got %d", len(msgs))
	}
	if params["system"] != "be terse" {
		t.Errorf("want system folded, got %v", params["system"])
	}

	asst := msgs[1].(map[string]any)["content"].([]any)
	var sawToolCall bool
	for _, b := range asst {
		bm := b.(map[string]any)
		if bm["type"] != "tool-call" {
			continue
		}
		sawToolCall = true
		if bm["toolCallId"] != "call_1" || bm["toolName"] != "get_weather" {
			t.Errorf("bad tool-call block: %v", bm)
		}
		input := bm["input"].(map[string]any)
		if input["city"] != "Jakarta" {
			t.Errorf("want parsed input, got %v", bm["input"])
		}
	}
	if !sawToolCall {
		t.Error("assistant tool_call not translated to tool-call block")
	}

	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" {
		t.Errorf("want role=tool, got %v", toolMsg["role"])
	}
	res := toolMsg["content"].([]any)[0].(map[string]any)
	if res["type"] != "tool-result" || res["toolName"] != "get_weather" {
		t.Errorf("bad tool-result block: %v", res)
	}
	if out, _ := res["output"].(map[string]any); out["type"] != "text" || out["value"] != "31C sunny" {
		t.Errorf("bad tool-result output: %v", res["output"])
	}
}

func TestBuildCommandcodeBody_ToolChoiceStrings(t *testing.T) {
	for in, want := range map[string]any{
		"required": map[string]any{"type": "any"},
		"auto":     map[string]any{"type": "auto"},
	} {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"` + in + `"}`)
		out, err := buildCommandcodeBody(body, "m")
		if err != nil {
			t.Fatalf("build(%s): %v", in, err)
		}
		var payload map[string]any
		json.Unmarshal(out, &payload)
		got := payload["params"].(map[string]any)["tool_choice"].(map[string]any)
		if got["type"] != want.(map[string]any)["type"] {
			t.Errorf("%s: want %v, got %v", in, want, got)
		}
	}
}

func TestProcessCommandcodeEvent_FinishToolCalls(t *testing.T) {
	state := &CommandcodeStreamState{ResponseID: "x", Created: 1}
	ProcessCommandcodeEvent(map[string]any{"type": "finish-step", "finishReason": "tool-calls"}, "finish-step", state)
	chunks := ProcessCommandcodeEvent(map[string]any{"type": "finish"}, "finish", state)
	combined := ""
	for _, c := range chunks {
		combined += c
	}
	if !strings.Contains(combined, `"finish_reason":"tool_calls"`) {
		t.Errorf("want finish_reason tool_calls, got %s", combined)
	}
}
