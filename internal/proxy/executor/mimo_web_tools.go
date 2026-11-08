package executor

import (
	"fmt"
	json "encoding/json/v2"
	"strings"
)

// Tool synthesis for the MiMo Studio web surface.
//
// The Studio chat has no native tool channel — the same constraint the
// DeepSeek web surface lives with — so, like that executor, this one
// teaches the declared tools as prompt text and recovers the calls from
// the answer with the shared extractor (deepSeekExtractToolCalls reads
// both the JSON object protocol and the DSML dialect). The prompt
// itself is mimo's own, for a measured reason: taught the JSON protocol
// the deepseek-web renderer asks for, MiMo does not emit the object —
// it narrates an attempted call and its failure ("the tool call
// failed") and answers nothing callable. Taught DSML, the dialect it
// reaches for natively, its invokes come back well-formed. So the
// renderer below teaches DSML as the protocol and keeps the JSON
// extraction as the fallback the shared extractor already provides.
// What is mimo's own is only the wiring and this renderer: where the
// block lands in the flattened query, how a past call is written back
// into the transcript, and how a recovered call is assembled into the
// reply.

// mimoWebRenderTools folds the declared tools into prompt text that
// teaches the DSML call block. The schema is emitted as JSON lines like
// the deepseek-web renderer's, because a model reproducing a schema it
// has only seen in prose returns a differently-shaped call more often
// than not. Every parameter value is written as plain text with
// string="true": the DSML parser carries strings, and a value that
// needs another JSON type is the client's to coerce from its schema.
func mimoWebRenderTools(tools []deepSeekToolSpec) string {
	var b strings.Builder
	b.WriteString("You have access to the following tools.\n\n<tools>\n")
	for _, tool := range tools {
		if tool.Function.Name == "" {
			continue
		}
		fmt.Fprintf(&b, "{\"name\": %q, \"description\": %q, \"parameters\": %s}\n",
			tool.Function.Name, tool.Function.Description,
			deepSeekSchemaText(tool.Function.Parameters))
	}
	b.WriteString("</tools>\n\n")
	b.WriteString("To use a tool, reply with a DSML calls block in exactly this shape, " +
		"with one invoke per call and nothing after the block:\n" +
		"<｜｜DSML｜｜ calls>\n" +
		"<｜｜DSML｜｜ invoke name=\"<tool name>\">\n" +
		"<｜｜DSML｜｜ parameter name=\"<parameter name>\" string=\"true\"><value as plain text></｜｜DSML｜｜ parameter>\n" +
		"</｜｜DSML｜｜ invoke>\n" +
		"</｜｜DSML｜｜ calls>\n")
	b.WriteString("\nRules for using tools:\n")
	// The same refusals the deepseek-web rules exist to prevent: a model
	// that reads its tool list as a description of what it lacks refuses
	// — or, as MiMo does, narrates a call it never made and reports the
	// narration as a failure. Stating the tools are installed and
	// runnable is what turns that into the call the user asked for.
	b.WriteString("- The tools above are installed and runnable. You can use them; a " +
		"request is not outside your abilities because it needs one.\n")
	b.WriteString("- Never answer that you lack a capability the list above provides, " +
		"and never describe a call as attempted or failed — either emit the calls " +
		"block so the call really runs, or answer in plain text.\n")
	b.WriteString("- If a request needs information you do not have, call a tool that " +
		"can fetch it before answering.\n")
	b.WriteString("- A question about the contents of a file, the output of a command, " +
		"or any other fact that lives outside this conversation IS a tool request: " +
		"emit the calls block. Answering such a question from memory, or narrating " +
		"an attempt and its failure, is an error.\n")
	b.WriteString("- A tool that runs commands executes on a real machine with network " +
		"access. \"I have no internet access\" is not a reason to refuse while such " +
		"a tool is declared — the tool has it, even when you do not.\n")
	b.WriteString("- A line reading \"[called <name> with <arguments>]\" means you already " +
		"made that call and its result is the following \"Tool result\" line. Do not repeat it.\n")
	b.WriteString("- Once you have a tool result for what was asked, answer the user in plain " +
		"text. Calling a tool again for the same information is an error.\n")
	b.WriteString("- A \"Tool result\" line is your only source of fact. Never state a " +
		"figure, date, temperature, price or count that is not in one of those lines, " +
		"and never present invented data as though a tool returned it.\n")
	b.WriteString("- When a tool result is empty, an error, or says nothing was found, " +
		"say so plainly. Do not substitute what you recall, and do not describe output " +
		"that was not returned.\n")
	b.WriteString("- Only call a tool when you actually need data you do not already have.\n")
	b.WriteString("- If several calls are needed, put them all in one reply rather than " +
		"answering one at a time.\n")
	return b.String()
}

// mimoWebParseTools reads the request's tool declarations. A request
// that opts out (tool_choice "none") or declares nothing usable is a
// plain chat request and takes the pre-synthesis path untouched.
func mimoWebParseTools(body map[string]any) []deepSeekToolSpec {
	if choice, _ := body["tool_choice"].(string); strings.EqualFold(choice, "none") {
		return nil
	}
	raw, ok := body["tools"]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) == 0 {
		return nil
	}
	var tools []deepSeekToolSpec
	if err := json.Unmarshal(encoded, &tools); err != nil {
		return nil
	}
	named := tools[:0]
	for _, tool := range tools {
		if tool.Function.Name != "" {
			named = append(named, tool)
		}
	}
	if len(named) == 0 {
		return nil
	}
	return named
}

// mimoWebAssistantText renders one assistant message for the flattened
// transcript. A turn that called tools is written back the way
// deepseek-web writes it: the prose with any call scaffolding removed,
// plus a "[called …]" line per call — without the line the replay shows
// a tool result with no cause and the model concludes it never called
// anything, then calls it again. An echoed call the client sends back
// as raw text (markup or a bare JSON object) is summarised instead of
// replayed verbatim, so the transcript never teaches the model that
// markup in the prose channel is how calls are made here.
func mimoWebAssistantText(msg map[string]any) string {
	text := mimoWebMessageText(msg)
	var rendered string
	if rawCalls, ok := msg["tool_calls"]; ok && rawCalls != nil {
		if encoded, err := json.Marshal(rawCalls); err == nil {
			rendered = deepSeekRenderToolCalls(encoded)
		}
	}
	kept := deepSeekDsmlStripped(text)
	for _, called := range deepSeekFindToolCallObjects(kept) {
		kept = deepSeekRemoveObject(kept, called)
	}
	kept = strings.TrimSpace(kept)
	if rendered != "" {
		if kept != "" {
			return kept + "\n" + rendered
		}
		return rendered
	}
	if kept != "" {
		return kept
	}
	if summary := deepSeekMarkupCallSummary(text); summary != "" {
		return summary
	}
	return text
}

// mimoWebFinalizeMessage turns a completed turn into the reply message
// when the request declared tools: calls are recovered from the answer
// text and returned as tool_calls with whatever prose surrounded them
// kept as the content, and an answer carrying markup but no recognisable
// call has the markup stripped rather than shown to the client as if it
// were the reply. Reasoning rides along on either shape.
func mimoWebFinalizeMessage(turn *mimoWebTurn, tools []deepSeekToolSpec) (map[string]any, string) {
	text := turn.content.String()
	calls := deepSeekExtractToolCalls(text, tools)
	var message map[string]any
	if len(calls) > 0 {
		raws := make([]string, 0, len(calls))
		for _, call := range calls {
			raws = append(raws, call.Raw)
		}
		message = deepSeekToolCallMessage(calls, deepSeekSurroundingText(text, raws...))
	} else {
		if deepSeekHasDSML(text) {
			text = deepSeekStripDSML(text)
		}
		message = map[string]any{"role": "assistant", "content": text}
	}
	if turn.reasoning.Len() > 0 {
		message["reasoning_content"] = turn.reasoning.String()
	}
	return message, deepSeekFinishReason(message)
}
