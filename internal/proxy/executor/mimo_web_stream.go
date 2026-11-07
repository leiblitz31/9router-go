package executor

import (
	"bufio"
	"context"
	json "encoding/json/v2"
	"io"
	"strings"
)

// The Studio's event stream, decoded.
//
// A turn answers as server-sent events. The names are the contract:
// `dialogId` hands back the server-side dialogue id (the value a chained
// follow-up turn would quote as its previousDialogueId), `message`
// carries answer deltas in `content` and thinking deltas in
// `reasoningContent`, `usage` carries the turn's token counts, `search`
// carries web citations when search ran, `finish` closes the turn, and
// `error` refuses it mid-stream. Frames are accumulated by the turn
// collector in mimo_web.go; this file only decodes.

type mimoWebEvent struct {
	Name string
	Data string
}

// mimoWebScanEvents decodes an SSE body into events until the stream
// ends, the context does, or the body fails. The callback returning false
// stops the scan early (the caller has what it needs or has failed).
func mimoWebScanEvents(ctx context.Context, body io.Reader, fn func(mimoWebEvent) bool) error {
	scanner := bufio.NewScanner(body)
	// Answer frames can carry long reasoning runs; the default 64K line
	// cap would cut them mid-turn.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var ev mimoWebEvent
	flush := func() bool {
		if ev.Name == "" && ev.Data == "" {
			return true
		}
		e := ev
		ev = mimoWebEvent{}
		return fn(e)
	}
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case strings.HasPrefix(line, "event:"):
			ev.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			if ev.Data != "" {
				ev.Data += "\n"
			}
			ev.Data += data
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	flush()
	return nil
}

// mimoWebMessagePayload is one `message` event's body.
type mimoWebMessagePayload struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoningContent"`
}

// mimoWebUsagePayload is the `usage` event's body.
type mimoWebUsagePayload struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
}

// mimoWebDialogID reads a `dialogId` event's body, which arrives either
// as a JSON object naming the id or as the bare id.
func mimoWebDialogID(data string) string {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "{") {
		return strings.Trim(trimmed, "\"")
	}
	var obj struct {
		DialogID   string `json:"dialogId"`
		DialogueID string `json:"dialogueId"`
		ID         string `json:"id"`
	}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return ""
	}
	switch {
	case obj.DialogID != "":
		return obj.DialogID
	case obj.DialogueID != "":
		return obj.DialogueID
	default:
		return obj.ID
	}
}

// mimoWebErrorMessage reads an `error` event's body into a sentence.
func mimoWebErrorMessage(data string) string {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return "the Studio refused the turn"
	}
	var obj struct {
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
		switch {
		case obj.Message != "":
			return obj.Message
		case obj.Msg != "":
			return obj.Msg
		case obj.Error != "":
			return obj.Error
		}
	}
	if len(trimmed) > 300 {
		trimmed = trimmed[:300]
	}
	return trimmed
}
