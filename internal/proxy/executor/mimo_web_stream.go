package executor

import (
	"bufio"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"strings"
	"time"
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

// mimoWebStreamIdleTimeout bounds how long a turn may go without a
// single byte from the Studio. A stream can stall mid-turn with the
// connection held open — the model neither answering nor erroring — and
// without this bound the scan (and the client behind it) waited for as
// long as the socket lived; a live agent turn sat visibly frozen for
// minutes on 2026-11-08 before completing on its own schedule. Healthy
// turns drip reasoning throughout, so two silent minutes is a stall,
// not a pause.
const mimoWebStreamIdleTimeout = 120 * time.Second

var errMimoWebStreamStalled = errors.New("mimo-web: the Studio stream stalled (no data for 120s)")

// mimoWebScanEvents decodes an SSE body into events until the stream
// ends, the context does, the body fails, or the stream stalls. The
// callback returning false stops the scan early (the caller has what it
// needs or has failed). The scanner runs on its own goroutine because a
// blocking read cannot also watch the idle timer.
func mimoWebScanEvents(ctx context.Context, body io.Reader, fn func(mimoWebEvent) bool) error {
	type scanLine struct {
		text string
		err  error
	}
	lines := make(chan scanLine, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(body)
		// Answer frames can carry long reasoning runs; the default 64K
		// line cap would cut them mid-turn.
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			select {
			case lines <- scanLine{text: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lines <- scanLine{err: err}:
			case <-ctx.Done():
			}
		}
	}()

	timer := time.NewTimer(mimoWebStreamIdleTimeout)
	defer timer.Stop()
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(mimoWebStreamIdleTimeout)
	}

	var ev mimoWebEvent
	flush := func() bool {
		if ev.Name == "" && ev.Data == "" {
			return true
		}
		e := ev
		ev = mimoWebEvent{}
		return fn(e)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errMimoWebStreamStalled
		case line, ok := <-lines:
			if !ok {
				flush()
				return nil
			}
			if line.err != nil {
				return line.err
			}
			resetTimer()
			switch {
			case line.text == "":
				if !flush() {
					return nil
				}
			case strings.HasPrefix(line.text, "event:"):
				ev.Name = strings.TrimSpace(strings.TrimPrefix(line.text, "event:"))
			case strings.HasPrefix(line.text, "data:"):
				data := strings.TrimPrefix(line.text, "data:")
				data = strings.TrimPrefix(data, " ")
				if ev.Data != "" {
					ev.Data += "\n"
				}
				ev.Data += data
			}
		}
	}
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
