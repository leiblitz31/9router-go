package executor

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Tool calling for qw-web (phase 2). chat.qwen.ai has no function-calling
// API; like every web-chat lane the protocol is prompt-injected and parsed
// back out of the answer text. The wire format is the one the Qwen wrapper
// ecosystem converged on after measuring the alternatives (Rfym21/Qwen2API,
// src/utils/agent-turn.js + tool-prompt.js):
//
//	[TOOL CALL]
//	{"name": "<tool_name>", "arguments": {<json object>}}
//	[END TOOL CALL]
//
// The Qwen-native <tool_call> JSON form is deliberately NOT taught: it is
// the platform's own format, and chat.qwen.ai's server-side agent loop
// intercepts it — the call is looked up in the platform's tool registry
// (which does not contain the client's tools), a "Tool <name> does not
// exists" note is injected into the generation, and the model abandons the
// call. Markers the platform does not recognise pass through untouched.
// The parser still ACCEPTS the angle-bracket form on the read side (models
// emit it from training habit when it does reach us), and tool results are
// rendered back with numbered markers plus an echo of the call's arguments:
//
//	[TOOL RESULT #2: get_weather]
//	Arguments: {"city": "Jakarta"}
//	<result text>
//	[END TOOL RESULT]
//
// The numbering is per-request, in call order across the whole history;
// the arguments echo is the belt-and-braces binding for calls that carry
// no usable id (the ds-web parallel-call rule).

// qwenWebToolDef is one entry of the request's `tools` array, in either
// the canonical {type:"function", function:{…}} shape or the flat
// {name, description, parameters} shape some clients send.
type qwenWebToolDef struct {
	Type     string `json:"type"`
	Function *struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  jsontext.Value `json:"parameters"`
	} `json:"function"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  jsontext.Value `json:"parameters"`
}

func (t qwenWebToolDef) resolved() (name, description string, parameters jsontext.Value, ok bool) {
	if t.Function != nil && t.Function.Name != "" {
		return t.Function.Name, t.Function.Description, t.Function.Parameters, true
	}
	if t.Name != "" {
		return t.Name, t.Description, t.Parameters, true
	}
	return "", "", nil, false
}

// qwenWebInputToolCall is one tool_call entry on an incoming assistant
// message (or the legacy single function_call).
type qwenWebInputToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function *struct {
		Name      string         `json:"name"`
		Arguments jsontext.Value `json:"arguments"`
	} `json:"function"`
	Name      string         `json:"name"`
	Arguments jsontext.Value `json:"arguments"`
}

func (c qwenWebInputToolCall) resolved() (id, name, args string) {
	id = c.ID
	if c.Function != nil {
		return id, c.Function.Name, qwenWebArgsText(c.Function.Arguments)
	}
	return id, c.Name, qwenWebArgsText(c.Arguments)
}

// qwenWebArgsText renders an incoming arguments value (a JSON string, a
// raw object, or nothing) as one compact JSON text for the transcript.
func qwenWebArgsText(raw jsontext.Value) string {
	if len(raw) == 0 {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if c, ok := qwenWebCanonicalJSON(s); ok {
			return c
		}
		return s
	}
	if c, ok := qwenWebCanonicalJSON(string(raw)); ok {
		return c
	}
	return string(raw)
}

// qwenWebToolChoice resolves the request's tool_choice: the string forms
// pass through; the object form yields the forced function name.
func qwenWebToolChoice(raw jsontext.Value) (mode, forcedName string) {
	if len(raw) == 0 {
		return "auto", ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, ""
	}
	var obj struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Function != nil && obj.Function.Name != "" {
			return "forced", obj.Function.Name
		}
		if obj.Name != "" {
			return "forced", obj.Name
		}
	}
	return "auto", ""
}

// qwenWebToolContext is the per-request tool state shared by the
// transcript renderer and the execute path. A nil context is the v1
// no-tools request, untouched.
type qwenWebToolContext struct {
	protocol   string
	allowed    map[string]bool
	mode       string // tool_choice resolution: "auto" | "required" | "forced" | other client strings
	forcedName string
}

// needsGate reports whether this request must not complete without a
// validated tool call (tool_choice "required" or a named function).
func (c *qwenWebToolContext) needsGate() bool {
	return c != nil && (c.mode == "required" || c.mode == "forced")
}

// qwenWebNewToolContext builds the context, or nil when the request does
// not activate tools (no tools, or tool_choice "none").
func qwenWebNewToolContext(tools []qwenWebToolDef, choiceRaw jsontext.Value) *qwenWebToolContext {
	mode, forced := qwenWebToolChoice(choiceRaw)
	if len(tools) == 0 || mode == "none" {
		return nil
	}
	allowed := map[string]bool{}
	for _, t := range tools {
		if name, _, _, ok := t.resolved(); ok {
			allowed[name] = true
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	return &qwenWebToolContext{
		protocol:   qwenWebToolProtocol(tools, mode, forced),
		allowed:    allowed,
		mode:       mode,
		forcedName: forced,
	}
}

// qwenWebToolProtocol renders the tool catalogue and the output contract
// into the instruction block that heads the flattened transcript. Adapted
// from Rfym21/Qwen2API's buildToolSystemPrompt (the wrapper with measured
// corpora on this exact upstream), trimmed to what a chat-completions
// gateway needs: no agent-harness wrappers, no retry prompts.
func qwenWebToolProtocol(tools []qwenWebToolDef, mode, forcedName string) string {
	var b strings.Builder
	b.WriteString("# Tools\n\nYou have access to the following tools. Use a tool when the task needs an action or information you do not have.\n\n## Available tools\n")
	for _, t := range tools {
		name, desc, params, ok := t.resolved()
		if !ok {
			continue
		}
		b.WriteString("- " + name + qwenWebSchemaSignature(params) + "\n")
		if d := qwenWebCompactText(desc, 280); d != "" {
			b.WriteString("  " + d + "\n")
		}
	}
	b.WriteString(`
## How to call a tool
Emit each call exactly in this form, with each marker on its own line:

[TOOL CALL]
{"name": "<tool_name>", "arguments": {<arguments as a JSON object>}}
[END TOOL CALL]

Rules:
- When you call a tool, the [TOOL CALL] block must be the first content of your reply. Do not write explanations before it; brief text after the closing marker is allowed.
- The JSON must be valid: double quotes, no trailing commas. "arguments" is always a JSON object ({} when there are no arguments).
- Use the exact tool name from the list above. Never invent a tool, and never print a [TOOL RESULT] block yourself.
- You may emit several [TOOL CALL] blocks in a row when more than one independent call is needed.
- Tool results return in the conversation as [TOOL RESULT #n: <tool_name>] blocks. The number matches the [TOOL CALL #n] it answers, and the call's arguments are repeated on the "Arguments:" line. Rely on the latest result for each call.
- If no tool is needed, answer in plain text with no markers at all.`)
	switch mode {
	case "required":
		b.WriteString("\n- You MUST call at least one tool before giving a plain-text answer.")
	case "forced":
		b.WriteString("\n- You MUST call the tool `" + forcedName + "` first.")
	}
	return b.String()
}

// qwenWebCompactText collapses whitespace and truncates a description for
// the catalogue, the wrapper way (measured token budget on this upstream).
func qwenWebCompactText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "…"
	}
	return s
}

// qwenWebSchema is the subset of JSON Schema the signature compressor
// reads. Property order is normalised alphabetically (the wrapper keeps
// declaration order; a deterministic order is worth more here than the
// client's incidental one).
type qwenWebSchema struct {
	Type        jsontext.Value            `json:"type"`
	Enum        []jsontext.Value          `json:"enum"`
	Properties  map[string]*qwenWebSchema `json:"properties"`
	Items       *qwenWebSchema            `json:"items"`
	Required    []string                  `json:"required"`
	Description string                    `json:"description"`
}

// qwenWebSchemaSignature renders a parameters schema as the compact
// signature that follows the tool name: { city: string; units?: "metric" | "imperial" }.
func qwenWebSchemaSignature(raw jsontext.Value) string {
	if len(raw) == 0 {
		return "{}"
	}
	var schema qwenWebSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return "{}"
	}
	return qwenWebSchemaType(&schema, 0)
}

func qwenWebSchemaType(s *qwenWebSchema, depth int) string {
	if s == nil {
		return "any"
	}
	if len(s.Enum) > 0 {
		vals := make([]string, 0, len(s.Enum))
		for _, v := range s.Enum {
			vals = append(vals, string(v))
		}
		return strings.Join(vals, " | ")
	}
	types := qwenWebSchemaTypes(s.Type)
	if len(types) > 1 {
		parts := make([]string, 0, len(types))
		for _, t := range types {
			clone := *s
			clone.Type = jsontext.Value(strconv.Quote(t))
			parts = append(parts, qwenWebSchemaType(&clone, depth))
		}
		return strings.Join(parts, " | ")
	}
	switch types[0] {
	case "array":
		return qwenWebSchemaType(s.Items, depth+1) + "[]"
	case "object":
		if len(s.Properties) == 0 || depth >= 3 {
			return "object"
		}
		required := map[string]bool{}
		for _, r := range s.Required {
			required[r] = true
		}
		keys := make([]string, 0, len(s.Properties))
		for k := range s.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make([]string, 0, len(keys))
		for _, k := range keys {
			prop := s.Properties[k]
			opt := ""
			if !required[k] {
				opt = "?"
			}
			field := fmt.Sprintf("%s%s: %s", k, opt, qwenWebSchemaType(prop, depth+1))
			if d := qwenWebCompactText(prop.Description, 120); d != "" {
				field += " /* " + strings.ReplaceAll(d, "*/", "* /") + " */"
			}
			fields = append(fields, field)
		}
		return "{ " + strings.Join(fields, "; ") + " }"
	default:
		return types[0]
	}
}

func qwenWebSchemaTypes(raw jsontext.Value) []string {
	if len(raw) == 0 {
		return []string{"any"}
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && one != "" {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil && len(many) > 0 {
		return many
	}
	return []string{"any"}
}
