package executor

import (
	"bufio"
	json "encoding/json/v2"
	"io"
	"strings"
)

// Qwen Web's completion stream is `data:`-only SSE carrying OpenAI-like
// chunks discriminated by choices[0].delta.phase:
//
//   - "think" / "thinking_summary": the reasoning. thinking_summary carries
//     no delta.content at all — the text lives in
//     delta.extra.summary_thought.content[] and each frame is a cumulative
//     SNAPSHOT of the summary so far (the live capture repeats whole frames
//     verbatim), so it must be merged by suffix, never appended blindly.
//   - "answer": the visible reply, in true incremental deltas.
//   - other phases (web_search, image_gen, …) carry no chat text in v1.
//
// A frame with delta.status "finished" ends the answer; the stream then
// closes without a [DONE] line. usage rides along cumulatively on most
// frames — the last one seen is the turn's usage. This file is the parser
// and accumulator; qwen_web.go owns the HTTP turn and the client-facing
// writer.

type qwenWebUsage struct {
	InputTokens   int `json:"input_tokens"`
	OutputTokens  int `json:"output_tokens"`
	TotalTokens   int `json:"total_tokens"`
	OutputDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
		TextTokens      int `json:"text_tokens"`
	} `json:"output_tokens_details"`
}

type qwenWebFrame struct {
	ResponseCreated *struct {
		ChatID     string `json:"chat_id"`
		ResponseID string `json:"response_id"`
	} `json:"response.created"`
	Choices []struct {
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			Phase   string `json:"phase"`
			Status  string `json:"status"`
			Extra   *struct {
				SummaryThought *struct {
					Content []string `json:"content"`
				} `json:"summary_thought"`
			} `json:"extra"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *qwenWebUsage `json:"usage"`
}

// qwenWebIsRiskControl reports whether a body is the risk-control refusal:
// HTTP 200 JSON {"ret":["FAIL_SYS_USER_VALIDATE","RGV587_ERROR::…"], …}
// instead of an event stream. It is the lane's signature silent failure —
// a reader that treated it as an empty stream would report a refused turn
// as a model that answered nothing.
func qwenWebIsRiskControl(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "RGV587") || strings.Contains(s, "FAIL_SYS_USER_VALIDATE")
}

// qwenWebAccumulator folds the frame stream into the turn's reasoning,
// answer and usage.
type qwenWebAccumulator struct {
	reasoning strings.Builder
	answer    strings.Builder
	usage     *qwenWebUsage
	finished  bool
}

// mergeSnapshot folds a cumulative snapshot into what has accumulated: when
// the incoming text restates the accumulation it contributes only its new
// suffix; when it does not, it is a fresh segment and is appended. This one
// rule serves both the thinking_summary snapshots and any think-phase frame
// that restates rather than extends.
func mergeSnapshot(acc *strings.Builder, incoming string) string {
	if incoming == "" {
		return ""
	}
	cur := acc.String()
	if strings.HasPrefix(incoming, cur) {
		delta := incoming[len(cur):]
		acc.WriteString(delta)
		return delta
	}
	if strings.HasPrefix(cur, incoming) {
		return "" // a restatement of a prefix already held: nothing new
	}
	acc.WriteString(incoming)
	return incoming
}

// apply folds one raw `data:` payload into the accumulator and returns the
// reasoning and answer deltas it produced.
func (a *qwenWebAccumulator) apply(payload []byte) (reasoningDelta, answerDelta string) {
	var frame qwenWebFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		return "", ""
	}
	if frame.Usage != nil {
		a.usage = frame.Usage
	}
	for _, choice := range frame.Choices {
		d := choice.Delta
		switch d.Phase {
		case "answer":
			answerDelta += d.Content
			a.answer.WriteString(d.Content)
		case "thinking_summary":
			if d.Extra != nil && d.Extra.SummaryThought != nil {
				reasoningDelta += mergeSnapshot(&a.reasoning, strings.Join(d.Extra.SummaryThought.Content, ""))
			}
			reasoningDelta += mergeSnapshot(&a.reasoning, d.Content) // normally empty
		case "think":
			reasoningDelta += mergeSnapshot(&a.reasoning, d.Content)
		}
		if d.Status == "finished" {
			a.finished = true
		}
	}
	return reasoningDelta, answerDelta
}

// qwenWebScanFrames reads an SSE body and hands each `data:` payload to fn,
// stopping early when fn returns false. Non-data lines (the blank frame
// separators, any keep-alive comment) are skipped.
func qwenWebScanFrames(r io.Reader, fn func(payload []byte) bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) < 5 || string(line[:5]) != "data:" {
			continue
		}
		payload := line[5:]
		for len(payload) > 0 && payload[0] == ' ' {
			payload = payload[1:]
		}
		if !fn(payload) {
			return nil
		}
	}
	return scanner.Err()
}
