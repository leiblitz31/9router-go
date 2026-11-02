package executor

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"9router/proxy/internal/proxy"
)

// qwenWebTurn runs one full upstream turn: create the chat, stream the
// completion, fold frames into the accumulator and forward deltas to
// onDelta as they arrive. A risk-control refusal at either stage refreshes
// the umid token, regenerates the fingerprint and retries the whole turn
// exactly once — the refusal names the token set, not the request, so one
// fresh set is the whole remedy v1 has; a second refusal is surfaced as a
// clean upstream error rather than retried into a throttle.
func qwenWebTurn(ctx context.Context, client *http.Client, req *Request, cred *qwenWebCredential, model string, messages []qwenWebWireMessage, onDelta func(reasoning, answer string) error) (*qwenWebAccumulator, error) {
	var lastErr error
	rgvRetried, authRetried := false, false
	for attempt := 0; attempt < 3; attempt++ {
		acc, err := qwenWebTurnOnce(ctx, client, model, messages, *cred, rgvRetried, onDelta)
		if err == nil {
			return acc, nil
		}
		var rc *qwenWebRiskControlError
		if errors.As(err, &rc) && !rgvRetried {
			rgvRetried = true
			lastErr = err
			continue
		}
		var ae *qwenWebAuthError
		if errors.As(err, &ae) && !cred.LoggedIn {
			return nil, qwenWebFailure(http.StatusUnauthorized, "qw-web: upstream returned 401 in guest mode")
		}
		if errors.As(err, &ae) && cred.LoggedIn && !authRetried {
			authRetried = true
			if rerr := qwenWebRefreshToken(ctx, client, req, cred); rerr != nil {
				return nil, qwenWebFailure(http.StatusUnauthorized,
					"qw-web: account token rejected (401) and re-minting from the stored cookie failed ("+rerr.Error()+") — paste a fresh token")
			}
			continue
		}
		return nil, err
	}
	return nil, qwenWebFailure(http.StatusBadGateway,
		"qw-web: risk control refused the request after a token refresh (RGV587) — "+lastErr.Error())
}

func qwenWebTurnOnce(ctx context.Context, client *http.Client, model string, messages []qwenWebWireMessage, cred qwenWebCredential, _ bool, onDelta func(reasoning, answer string) error) (*qwenWebAccumulator, error) {
	bxUA := qwenWebBXUA()
	// A fresh umid per turn, not the shared cache: every verified pass of
	// this lane (the reference wrappers' runs and this gateway's live
	// tests) paired one freshly fetched umid with one chat, while a umid
	// reused across many chats in one process drew refusals. The cache
	// remains as the fetch's fallback and for the auth paths.
	umid := qwenWebUMIDToken(ctx, client, true)
	headers := qwenWebHeaders(bxUA, umid)
	qwenWebApplyAuth(headers, cred)

	chatID, err := qwenWebCreateChat(ctx, client, headers, model, messages)
	if err != nil {
		return nil, err
	}

	last := messages[len(messages)-1]
	body := map[string]any{
		"stream":             true,
		"version":            "2.1",
		"incremental_output": true,
		"chatId":             chatID,
		"chat_id":            chatID,
		"chat_mode":          "normal",
		"model":              model,
		"messages":           messages,
		"timestamp":          time.Now().Unix(),
	}
	if last.ParentID != nil {
		body["parentId"] = last.ParentID
		body["parent_id"] = last.ParentID
	} else {
		body["parentId"] = ""
		body["parent_id"] = nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("qw-web: marshal completion: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		qwenWebBaseURL+"/api/v2/chat/completions?chat_id="+chatID, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("qw-web: build completion request: %w", err)
	}
	req.Header = headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return nil, qwenWebFailure(http.StatusBadGateway, "qw-web: completion request: "+err.Error())
	}
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode == http.StatusUnauthorized && cred.LoggedIn {
			return nil, &qwenWebAuthError{stage: "completions"}
		}
		if qwenWebIsRiskControl(snippet) {
			return nil, &qwenWebRiskControlError{stage: "completions"}
		}
		if resp.StatusCode != http.StatusOK {
			return nil, qwenWebFailure(resp.StatusCode, "qw-web: completion refused: "+qwenWebSnippet(snippet))
		}
		return nil, qwenWebFailure(http.StatusBadGateway, "qw-web: completion answered without an event stream: "+qwenWebSnippet(snippet))
	}

	acc := &qwenWebAccumulator{}
	scanErr := qwenWebScanFrames(resp.Body, func(payload []byte) bool {
		if qwenWebIsRiskControl(payload) {
			return false
		}
		rd, ad := acc.apply(payload)
		if (rd != "" || ad != "") && onDelta != nil {
			_ = onDelta(rd, ad)
		}
		return true
	})
	if scanErr != nil {
		return nil, qwenWebFailure(http.StatusBadGateway, "qw-web: read completion stream: "+scanErr.Error())
	}
	if acc.answer.Len() == 0 && acc.reasoning.Len() == 0 {
		return nil, qwenWebFailure(http.StatusBadGateway, "qw-web: stream ended without an answer")
	}
	return acc, nil
}

// qwenWebCreateChat opens the upstream chat and returns its id. The title
// follows the site's own convention: the first user message, truncated.
func qwenWebCreateChat(ctx context.Context, client *http.Client, headers http.Header, model string, messages []qwenWebWireMessage) (string, error) {
	title := "New Chat"
	for _, m := range messages {
		if m.Role == "user" && m.Content != "" {
			title = m.Content
			if len(title) > 40 {
				title = title[:40]
			}
			break
		}
	}
	payload := map[string]any{
		"title":     title,
		"models":    []string{model},
		"chat_mode": "normal",
		"chat_type": "t2t",
		"timestamp": time.Now().Unix(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("qw-web: marshal chats/new: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qwenWebBaseURL+"/api/v2/chats/new", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("qw-web: build chats/new request: %w", err)
	}
	req.Header = headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return "", qwenWebFailure(http.StatusBadGateway, "qw-web: chats/new request: "+err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return "", qwenWebFailure(http.StatusBadGateway, "qw-web: read chats/new: "+err.Error())
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return "", &qwenWebAuthError{stage: "chats/new"}
	}
	if qwenWebIsRiskControl(body) {
		return "", &qwenWebRiskControlError{stage: "chats/new"}
	}
	if resp.StatusCode != http.StatusOK {
		return "", qwenWebFailure(resp.StatusCode, "qw-web: chats/new refused: "+qwenWebSnippet(body))
	}
	var created struct {
		Success bool `json:"success"`
		Data    struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.Data.ID == "" {
		return "", qwenWebFailure(http.StatusBadGateway, "qw-web: chats/new returned no chat id: "+qwenWebSnippet(body))
	}
	return created.Data.ID, nil
}

func qwenWebSnippet(b []byte) string {
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// qwenWebExecute owns the client-facing side of the turn: it streams
// OpenAI chunks when the client asked for a stream (headers are committed
// only when the first delta exists, so a turn that fails early still
// surfaces its real status), and otherwise drains the turn into one
// chat.completion body.
func qwenWebExecute(w http.ResponseWriter, req *Request, ctx context.Context, client *http.Client, cred qwenWebCredential, model string, messages []qwenWebWireMessage, thinking bool) error {
	if cred.LoggedIn {
		if err := qwenWebEnsureAuth(ctx, client, req, &cred); err != nil {
			return err
		}
	}
	emitter := &qwenWebEmitter{w: w, req: req, model: model, id: "chatcmpl-qw-" + strconv.FormatInt(time.Now().UnixNano(), 36), created: time.Now().Unix()}
	acc, err := qwenWebTurn(ctx, client, req, &cred, model, messages, func(rd, ad string) error {
		if !req.IsStream {
			return nil // deltas are folded into acc; the body is written whole below
		}
		return emitter.delta(rd, ad)
	})
	if err != nil {
		if emitter.opened {
			emitter.errorFrame(err.Error())
			return nil
		}
		return err
	}
	if req.IsStream {
		return emitter.finish(acc)
	}
	return emitter.writeWhole(acc)
}

type qwenWebEmitter struct {
	w       http.ResponseWriter
	req     *Request
	model   string
	id      string
	created int64
	flusher http.Flusher
	opened  bool
}

func (e *qwenWebEmitter) open() {
	if e.opened {
		return
	}
	e.opened = true
	e.flusher = proxy.WriteSSEHeaders(e.w)
	e.frame(map[string]any{"role": "assistant", "content": ""}, nil, nil)
}

func (e *qwenWebEmitter) frame(delta map[string]any, finish any, usage *qwenWebUsage) {
	chunk := map[string]any{
		"id":      e.id,
		"object":  "chat.completion.chunk",
		"created": e.created,
		"model":   e.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		chunk["usage"] = qwenWebUsageJSON(usage)
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	_, _ = e.w.Write([]byte("data: "))
	_, _ = e.w.Write(raw)
	_, _ = e.w.Write([]byte("\n\n"))
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

func (e *qwenWebEmitter) delta(reasoning, answer string) error {
	e.open()
	if reasoning != "" {
		e.frame(map[string]any{"reasoning_content": reasoning}, nil, nil)
	}
	if answer != "" {
		if e.req.ResponseBuf != nil {
			_, _ = e.req.ResponseBuf.Write([]byte(answer))
		}
		e.frame(map[string]any{"content": answer}, nil, nil)
	}
	return nil
}

func (e *qwenWebEmitter) finish(acc *qwenWebAccumulator) error {
	e.open()
	e.frame(map[string]any{}, "stop", acc.usage)
	_, _ = e.w.Write([]byte("data: [DONE]\n\n"))
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

func (e *qwenWebEmitter) errorFrame(message string) {
	raw, err := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}})
	if err == nil {
		_, _ = e.w.Write([]byte("data: "))
		_, _ = e.w.Write(raw)
		_, _ = e.w.Write([]byte("\n\n"))
	}
	_, _ = e.w.Write([]byte("data: [DONE]\n\n"))
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

func (e *qwenWebEmitter) writeWhole(acc *qwenWebAccumulator) error {
	message := map[string]any{"role": "assistant", "content": acc.answer.String()}
	if acc.reasoning.Len() > 0 {
		message["reasoning_content"] = acc.reasoning.String()
	}
	body := map[string]any{
		"id":      e.id,
		"object":  "chat.completion",
		"created": e.created,
		"model":   e.model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": "stop"}},
	}
	if acc.usage != nil {
		body["usage"] = qwenWebUsageJSON(acc.usage)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("qw-web: marshal response: %w", err)
	}
	if e.req.ResponseBuf != nil {
		_, _ = e.req.ResponseBuf.Write([]byte(acc.answer.String()))
	}
	e.w.Header().Set("Content-Type", "application/json")
	e.w.WriteHeader(http.StatusOK)
	_, err = e.w.Write(raw)
	return err
}

func qwenWebUsageJSON(u *qwenWebUsage) map[string]any {
	out := map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.OutputDetails.ReasoningTokens > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.OutputDetails.ReasoningTokens}
	}
	return out
}
