package executor

import "strings"

// OpenCode free-tier CLI environment marker (decolua/9router#5218 fix #1,
// #5222): the upstream gate requires the effective system prompt to carry
// this <env> block and answers 429 FreeUsageLimitError without it — even with
// correct headers, UA and session format. The marker is injected into the oc
// free lane only (ForwardOpencode, forwardOpencodeGoFree); the keyed lanes
// (opencode-zen, opencode-go) never see it.

const openCodeEnvBlock = "<env>\n  Working directory: /workspace\n  Workspace root folder: /workspace\n</env>"

const (
	openCodeEnvWorkingDirLine = "Working directory:"
	openCodeEnvWorkspaceLine  = "Workspace root folder:"
)

// openCodeEnvSpan returns the span of the first <env>...</env> block in text.
// When the closing tag is missing, end is len(text); when there is no block
// at all, start is -1.
func openCodeEnvSpan(text string) (start, end int) {
	start = strings.Index(text, "<env>")
	if start < 0 {
		return -1, -1
	}
	if close := strings.Index(text[start:], "</env>"); close >= 0 {
		return start, start + close + len("</env>")
	}
	return start, len(text)
}

// ensureOpenCodeEnvMarkerText returns text carrying a complete <env> block:
// untouched when one is already present, completed in place when partial,
// appended when absent. It never adds a second block.
func ensureOpenCodeEnvMarkerText(text string) string {
	start, end := openCodeEnvSpan(text)
	if start < 0 {
		if text == "" {
			return openCodeEnvBlock
		}
		return text + "\n\n" + openCodeEnvBlock
	}
	block := text[start:end]
	var missing []string
	if !strings.Contains(block, openCodeEnvWorkingDirLine) {
		missing = append(missing, "  Working directory: /workspace")
	}
	if !strings.Contains(block, openCodeEnvWorkspaceLine) {
		missing = append(missing, "  Workspace root folder: /workspace")
	}
	if len(missing) == 0 {
		if strings.Contains(block, "</env>") {
			return text
		}
		// Complete lines but unclosed: close the block.
		if strings.HasSuffix(text, "\n") {
			return text + "</env>"
		}
		return text + "\n</env>"
	}
	insert := strings.Join(missing, "\n")
	if closeIdx := strings.Index(block, "</env>"); closeIdx >= 0 {
		at := start + closeIdx
		return text[:at] + insert + "\n" + text[at:]
	}
	// Unclosed partial block: append the missing lines and close it.
	sep := "\n"
	if strings.HasSuffix(text, "\n") {
		sep = ""
	}
	return text + sep + insert + "\n</env>"
}

// ensureOpenCodeEnvMarkerParts injects the marker into the first text part of
// a content-block array, or appends a text part carrying only the block when
// no text part exists. The possibly-grown slice is returned so callers can
// store it back.
func ensureOpenCodeEnvMarkerParts(parts []any) []any {
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := pm["type"].(string)
		if typ != "text" && typ != "input_text" {
			continue
		}
		if txt, ok := pm["text"].(string); ok {
			pm["text"] = ensureOpenCodeEnvMarkerText(txt)
			return parts
		}
	}
	return append(parts, map[string]any{"type": "text", "text": openCodeEnvBlock})
}

// ensureOpenCodeEnvMarker injects the CLI <env> block into every system-prompt
// shape an oc free-lane body may carry: OpenAI messages[] (first system
// message, or a prepended one when none exists), a Claude top-level system
// (string or blocks), and Responses instructions. A body carrying none of
// these is left untouched, and an unknown content shape is never corrupted.
func ensureOpenCodeEnvMarker(reqMap map[string]any) {
	if msgs, ok := reqMap["messages"].([]any); ok {
		found := false
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok || mm["role"] != "system" {
				continue
			}
			found = true
			switch c := mm["content"].(type) {
			case string:
				mm["content"] = ensureOpenCodeEnvMarkerText(c)
			case []any:
				mm["content"] = ensureOpenCodeEnvMarkerParts(c)
			case nil:
				mm["content"] = openCodeEnvBlock
			}
			break
		}
		if !found {
			sys := map[string]any{"role": "system", "content": openCodeEnvBlock}
			reqMap["messages"] = append([]any{sys}, msgs...)
		}
	}
	if sys, ok := reqMap["system"]; ok {
		switch s := sys.(type) {
		case string:
			reqMap["system"] = ensureOpenCodeEnvMarkerText(s)
		case []any:
			reqMap["system"] = ensureOpenCodeEnvMarkerParts(s)
		}
	}
	if ins, ok := reqMap["instructions"].(string); ok {
		reqMap["instructions"] = ensureOpenCodeEnvMarkerText(ins)
		return
	}
	if _, hasInput := reqMap["input"]; hasInput {
		if _, ok := reqMap["instructions"]; !ok {
			reqMap["instructions"] = openCodeEnvBlock
		}
	}
}
