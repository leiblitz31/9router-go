package executor

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"9router/proxy/internal/log"
	"9router/proxy/internal/proxy"
)

// ForwardMimoWeb routes a chat completion to Xiaomi MiMo Studio
// (aistudio.xiaomimimo.com), the signed-in web chat, over plain HTTP —
// no browser, no sidecar. The connection's key is the captured Studio
// session (see mimo_web_client.go); the executor flattens the client's
// messages into the single query the web turn takes, opens a fresh
// server-side conversation per request so a stateless router request is
// self-contained, and translates the site's event stream back into
// OpenAI chunks as it arrives.
//
// What the web surface does not have is a native tool channel, so tool
// support is synthesised the way the DeepSeek web executor synthesises
// it: the declared tools are taught as prompt text and the calls are
// recovered from the answer (see mimo_web_tools.go). A request without
// tools is answered as plain chat, exactly as before.
func ForwardMimoWeb(w http.ResponseWriter, req *Request) error {
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil || body == nil {
		return proxy.UpstreamFailure(http.StatusBadRequest, "mimo-web: request body is not a JSON object")
	}
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		return proxy.UpstreamFailure(http.StatusBadRequest, "mimo-web: missing or empty messages array")
	}
	tools := mimoWebParseTools(body)
	query := mimoWebFlattenMessages(messages, tools)
	if strings.TrimSpace(query) == "" {
		return proxy.UpstreamFailure(http.StatusBadRequest, "mimo-web: no text content in messages")
	}
	if len(tools) > 0 {
		query = mimoWebRenderTools(tools) + "\n\n" + query
	}

	creds, err := mimoWebParseCredentials(req.APIKey)
	if err != nil {
		return proxy.UpstreamFailure(http.StatusBadRequest, err.Error())
	}
	model := mimoWebResolveModel(req, body)
	client := newMimoWebClient(req.Client, creds)

	turn := mimoWebChatBody{
		MsgID:          mimoWebRandomID(),
		ConversationID: mimoWebRandomID(),
		Query:          query,
		IsEditedQuery:  false,
		SceneType:      nil,
		Params:         map[string]any{},
		ModelConfig: mimoWebModelConfig{
			EnableThinking:  mimoWebThinkingEnabled(body),
			WebSearchStatus: mimoWebSearchStatus(body),
			Model:           model,
			Temperature:     mimoWebFloatParam(body, "temperature"),
			TopP:            mimoWebFloatParam(body, "top_p"),
			ReasoningEffort: mimoWebReasoningEffort(body),
		},
		MultiMedias: []any{},
		Attachments: []any{},
	}

	responseID := "chatcmpl-mimo-" + mimoWebRandomID()[:12]
	if len(tools) > 0 {
		return mimoWebToolTurn(ctx, w, req, client, turn, model, responseID, tools)
	}

	log.Info("mimo-web", "query", "model", model, "chars", len(query), "thinking", turn.ModelConfig.EnableThinking)

	stream, err := client.chat(ctx, turn)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	// The page registers the conversation on the account after opening
	// the turn; doing the same keeps the account's history shaped like
	// real use. Fire and forget on a detached context: the save must
	// outlive neither the turn nor its failures, and never block it.
	go client.saveConversation(context.WithoutCancel(ctx), turn.ConversationID, query, mimoWebIsUltra(model))

	if req.IsStream {
		return mimoWebStreamToClient(ctx, w, req, stream, model, responseID)
	}
	return mimoWebCollectToClient(ctx, w, req, stream, model, responseID)
}

// mimoWebResolveModel takes the bare model id for this turn: the router's
// own extraction first, the body's model second, with any provider
// prefix the client wrote stripped either way.
func mimoWebResolveModel(req *Request, body map[string]any) string {
	model := strings.TrimSpace(req.ModelName)
	if model == "" {
		model, _ = body["model"].(string)
		model = strings.TrimSpace(model)
	}
	for _, prefix := range []string{"mimo-web/", "mw/"} {
		model = strings.TrimPrefix(model, prefix)
	}
	if model == "" {
		model = "mimo-v2.6-flash"
	}
	return model
}

// mimoWebThinkingEnabled: the Studio's own default for the current
// models is thinking on, so a request has to opt out, not in — an
// explicit reasoning_effort of "none" or a disabled thinking block does.
func mimoWebThinkingEnabled(body map[string]any) bool {
	if effort, _ := body["reasoning_effort"].(string); strings.EqualFold(effort, "none") {
		return false
	}
	if thinking, ok := body["thinking"].(map[string]any); ok {
		if t, _ := thinking["type"].(string); strings.EqualFold(t, "disabled") {
			return false
		}
	}
	return true
}

// mimoWebReasoningEffort passes a client's effort tier through; with
// thinking on and no tier named, the upstream applies its own default.
func mimoWebReasoningEffort(body map[string]any) string {
	effort, _ := body["reasoning_effort"].(string)
	effort = strings.ToLower(strings.TrimSpace(effort))
	switch effort {
	case "low", "medium", "high":
		return effort
	}
	return ""
}

// mimoWebSearchStatus mirrors the Studio's search toggle: off unless the
// request asks for it, then "auto" (the model decides), like the page.
func mimoWebSearchStatus(body map[string]any) string {
	for _, key := range []string{"web_search", "enable_search"} {
		if on, ok := body[key].(bool); ok && on {
			return "auto"
		}
	}
	return "disabled"
}

func mimoWebFloatParam(body map[string]any, key string) *float64 {
	if v, ok := body[key].(float64); ok {
		return &v
	}
	return nil
}

// mimoWebFlattenMessages renders a chat transcript as the single query
// the web turn takes. A lone user message goes verbatim — the common
// case stays exactly what the user typed. Anything longer is labelled
// by role so the model can tell the speakers apart. Media parts are
// noted, not dropped silently: the upload flow is a separate request
// this executor does not make yet.
func mimoWebFlattenMessages(messages []any, tools []deepSeekToolSpec) string {
	if len(messages) == 1 && len(tools) == 0 {
		if msg, ok := messages[0].(map[string]any); ok {
			if role, _ := msg["role"].(string); role == "" || role == "user" {
				return mimoWebMessageText(msg)
			}
		}
	}
	var b strings.Builder
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		text := mimoWebMessageText(msg)
		if role == "assistant" && len(tools) > 0 {
			// A tool-calling transcript has to show the calls themselves;
			// the plain text of an assistant turn omits them entirely.
			text = mimoWebAssistantText(msg)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		switch role {
		case "system":
			b.WriteString("System: ")
		case "assistant":
			b.WriteString("Assistant: ")
		case "tool", "function":
			b.WriteString("Tool result: ")
		default:
			b.WriteString("User: ")
		}
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// mimoWebMessageText extracts the text of one message: a plain string
// body, or the text parts of a content array.
func mimoWebMessageText(msg map[string]any) string {
	switch content := msg["content"].(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "text", "input_text", "":
				if t, ok := part["text"].(string); ok {
					parts = append(parts, t)
				}
			case "image_url", "image", "input_image":
				parts = append(parts, "[image attached]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// mimoWebTurn accumulates one upstream turn while it streams.
type mimoWebTurn struct {
	content   strings.Builder
	reasoning strings.Builder
	usage     *mimoWebUsagePayload
	dialogID  string
	errText   string
}

func (t *mimoWebTurn) apply(ev mimoWebEvent, onDelta func(reasoningDelta, contentDelta string)) {
	switch ev.Name {
	case "dialogId":
		t.dialogID = mimoWebDialogID(ev.Data)
	case "message":
		var payload mimoWebMessagePayload
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			return
		}
		if payload.ReasoningContent != "" {
			t.reasoning.WriteString(payload.ReasoningContent)
		}
		if payload.Content != "" {
			t.content.WriteString(payload.Content)
		}
		if onDelta != nil && (payload.ReasoningContent != "" || payload.Content != "") {
			onDelta(payload.ReasoningContent, payload.Content)
		}
	case "usage":
		var payload mimoWebUsagePayload
		if err := json.Unmarshal([]byte(ev.Data), &payload); err == nil {
			t.usage = &payload
		}
	case "error":
		t.errText = mimoWebErrorMessage(ev.Data)
	}
}

func (t *mimoWebTurn) usageMap() map[string]any {
	if t.usage == nil {
		return nil
	}
	total := t.usage.TotalTokens
	if total == 0 {
		total = t.usage.PromptTokens + t.usage.CompletionTokens
	}
	return map[string]any{
		"prompt_tokens":     t.usage.PromptTokens,
		"completion_tokens": t.usage.CompletionTokens,
		"total_tokens":      total,
	}
}

func mimoWebChunk(responseID, model string, created int64, delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			},
		},
	}
}

// mimoWebStreamToClient relays the turn as OpenAI chunks, reasoning and
// content on their own delta fields, closing with the usage the upstream
// reported (when it reported any). Headers are committed only when the
// first answer delta exists, so a turn the upstream refuses up front
// still surfaces as the failure it is instead of an empty 200 stream.
func mimoWebStreamToClient(ctx context.Context, w http.ResponseWriter, req *Request, stream io.ReadCloser, model, responseID string) error {
	turn := &mimoWebTurn{}
	created := time.Now().Unix()
	flusher, _ := w.(http.Flusher)
	started := false

	writeChunk := func(chunk map[string]any) {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			started = true
			_ = writeSSE(w, mimoWebChunk(responseID, model, created, map[string]any{"role": "assistant"}, nil))
		}
		_ = writeSSE(w, chunk)
		if flusher != nil {
			flusher.Flush()
		}
	}

	err := mimoWebScanEvents(ctx, stream, func(ev mimoWebEvent) bool {
		turn.apply(ev, func(reasoningDelta, contentDelta string) {
			if req.TTFT != nil && *req.TTFT == 0 && !req.StartTime.IsZero() {
				*req.TTFT = time.Since(req.StartTime).Milliseconds()
			}
			delta := map[string]any{}
			if reasoningDelta != "" {
				delta["reasoning_content"] = reasoningDelta
			}
			if contentDelta != "" {
				delta["content"] = contentDelta
			}
			writeChunk(mimoWebChunk(responseID, model, created, delta, nil))
		})
		return turn.errText == ""
	})
	if err != nil {
		if !started {
			return proxy.UpstreamFailure(http.StatusBadGateway, "mimo-web: stream failed: "+err.Error())
		}
		return nil
	}
	if turn.errText != "" {
		if !started {
			return proxy.UpstreamFailure(http.StatusBadGateway, "mimo-web: "+turn.errText)
		}
		return nil
	}
	if !started {
		return proxy.UpstreamFailure(http.StatusBadGateway, "mimo-web: the Studio ended the stream without an answer")
	}
	_ = writeSSE(w, mimoWebChunk(responseID, model, created, map[string]any{}, "stop"))
	if usage := turn.usageMap(); usage != nil {
		_ = writeSSE(w, map[string]any{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   usage,
		})
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// mimoWebToolTurn serves a request that declared tools. An attempt is
// one full Studio round trip; the turn is buffered whole before anything
// is written, because a call written as JSON or DSML markup cannot be
// recognised — or kept out of the client's content channel — until the
// answer is complete, and an agent client waits for the call rather
// than reading prose token by token. The account history registration
// happens once, for the first attempt, as it would for a single turn.
func mimoWebToolTurn(ctx context.Context, w http.ResponseWriter, req *Request, client *mimoWebClient, template mimoWebChatBody, model, responseID string, tools []deepSeekToolSpec) error {
	firstConversation := template.ConversationID
	saved := false
	round := func(query string) (*mimoWebTurn, error) {
		body := template
		body.MsgID = mimoWebRandomID()
		body.Query = query
		if saved {
			body.ConversationID = mimoWebRandomID()
		}
		log.Info("mimo-web", "query", "model", model, "chars", len(query), "thinking", body.ModelConfig.EnableThinking)
		stream, err := client.chat(ctx, body)
		if err != nil {
			return nil, err
		}
		defer func() { _ = stream.Close() }()
		if !saved {
			saved = true
			go client.saveConversation(context.WithoutCancel(ctx), firstConversation, template.Query, mimoWebIsUltra(model))
		}
		turn := &mimoWebTurn{}
		if err := mimoWebScanEvents(ctx, stream, func(ev mimoWebEvent) bool {
			turn.apply(ev, nil)
			return turn.errText == ""
		}); err != nil {
			return nil, proxy.UpstreamFailure(http.StatusBadGateway, "mimo-web: stream failed: "+err.Error())
		}
		if turn.errText != "" {
			return nil, proxy.UpstreamFailure(http.StatusBadGateway, "mimo-web: "+turn.errText)
		}
		if turn.content.Len() == 0 && turn.reasoning.Len() == 0 {
			return nil, proxy.UpstreamFailure(http.StatusBadGateway, "mimo-web: the Studio ended the stream without an answer")
		}
		return turn, nil
	}
	turn, calls, err := mimoWebAttemptWithRetry(round, template.Query, tools)
	if err != nil {
		return err
	}
	if req.TTFT != nil && *req.TTFT == 0 && !req.StartTime.IsZero() {
		*req.TTFT = time.Since(req.StartTime).Milliseconds()
	}
	message, finish := mimoWebFinalizeMessage(turn, calls)
	if req.IsStream {
		return mimoWebWriteStreamResult(w, model, responseID, turn, message, finish)
	}
	return mimoWebWriteCompletion(w, model, responseID, turn, message, finish)
}

// mimoWebAttemptWithRetry runs one tool turn and validates the calls it
// recovers. A malformed call — the observed case is arguments that lost
// their required properties — used to be forwarded as-is, and the client
// rejected it, failing a turn that was one correction away from working.
// So an attempt carrying an invalid call is re-asked exactly once with
// the defect named (mimoWebCorrectionNote); if the retry fails outright
// or still yields nothing valid, the first attempt stands with only its
// valid calls kept. A retry that answers in prose instead of calling is
// accepted as the model's answer.
func mimoWebAttemptWithRetry(round func(query string) (*mimoWebTurn, error), query string, tools []deepSeekToolSpec) (*mimoWebTurn, []deepSeekToolRequest, error) {
	turn, err := round(query)
	if err != nil {
		return nil, nil, err
	}
	calls := deepSeekExtractToolCalls(turn.content.String(), tools)
	invalid := mimoWebValidateCalls(calls, tools)
	if len(invalid) == 0 {
		if len(calls) == 0 && mimoWebNarratesToolFailure(turn.content.String()) {
			return mimoWebRetryNarration(round, query, turn, tools)
		}
		return turn, calls, nil
	}
	log.Info("mimo-web", "recovered calls failed validation; one corrective retry", "invalid", len(invalid))
	retry, err := round(query + mimoWebCorrectionNote(invalid))
	if err != nil {
		return turn, mimoWebValidCalls(calls, tools), nil
	}
	retryCalls := deepSeekExtractToolCalls(retry.content.String(), tools)
	if valid := mimoWebValidCalls(retryCalls, tools); len(valid) > 0 {
		return retry, valid, nil
	}
	if len(retryCalls) == 0 {
		return retry, nil, nil
	}
	return turn, mimoWebValidCalls(calls, tools), nil
}

// mimoWebRetryNarration re-asks a turn that narrated tool failure
// without calling. A retry that produces valid calls — or an honest
// prose answer — replaces the narration; anything else leaves the first
// attempt standing, so the retry can never make the turn worse.
func mimoWebRetryNarration(round func(query string) (*mimoWebTurn, error), query string, first *mimoWebTurn, tools []deepSeekToolSpec) (*mimoWebTurn, []deepSeekToolRequest, error) {
	log.Info("mimo-web", "answer narrated tool failure without a call; one corrective retry")
	retry, err := round(query + mimoWebNarrationNote())
	if err != nil {
		return first, nil, nil
	}
	retryCalls := deepSeekExtractToolCalls(retry.content.String(), tools)
	if valid := mimoWebValidCalls(retryCalls, tools); len(valid) > 0 {
		return retry, valid, nil
	}
	if len(retryCalls) == 0 && !mimoWebNarratesToolFailure(retry.content.String()) {
		return retry, nil, nil
	}
	return first, nil, nil
}

// mimoWebNarratesToolFailure reports whether an answer claims tool calls
// failed without having made any — the model's other synthesis failure,
// seen live on install-shaped tasks: it writes that the bash tool "is
// failing on every call" and invents the attempts (down to an `echo hi`
// that never ran) instead of emitting a call. The phrases are the ones
// the model actually produces, in English and in the Studio's Chinese.
func mimoWebNarratesToolFailure(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{
		"工具调用失败",
		"tool call failed",
		"tool calls failed",
		"tool is failing",
		"tools are failing",
		"tool calls are failing",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// mimoWebNarrationNote is the correction appended when a turn narrated
// tool failure instead of calling: it states plainly that no call ran,
// so nothing has failed yet, and asks for the calls block itself.
func mimoWebNarrationNote() string {
	return "\n\n[Correction: your previous reply described tool calls as failing " +
		"without emitting any call. No call was run, so nothing has failed yet. " +
		"Emit the DSML calls block for the tool call(s) this task needs, with " +
		"every required parameter filled in.]"
}

// mimoWebWriteStreamResult emits a buffered tool turn as OpenAI chunks:
// reasoning and content as single deltas, then the finish chunk and the
// usage the upstream reported.
func mimoWebWriteStreamResult(w http.ResponseWriter, model, responseID string, turn *mimoWebTurn, message map[string]any, finish string) error {
	created := time.Now().Unix()
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_ = writeSSE(w, mimoWebChunk(responseID, model, created, map[string]any{"role": "assistant"}, nil))
	delta := map[string]any{}
	if reasoning, _ := message["reasoning_content"].(string); reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	if content, _ := message["content"].(string); content != "" {
		delta["content"] = content
	}
	if calls, ok := message["tool_calls"]; ok {
		delta["tool_calls"] = calls
	}
	if len(delta) > 0 {
		_ = writeSSE(w, mimoWebChunk(responseID, model, created, delta, nil))
	}
	_ = writeSSE(w, mimoWebChunk(responseID, model, created, map[string]any{}, finish))
	if usage := turn.usageMap(); usage != nil {
		_ = writeSSE(w, map[string]any{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   usage,
		})
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// mimoWebCollectToClient buffers a plain turn and answers one
// completion. Tool turns take mimoWebToolTurn instead.
func mimoWebCollectToClient(ctx context.Context, w http.ResponseWriter, req *Request, stream io.ReadCloser, model, responseID string) error {
	turn := &mimoWebTurn{}
	err := mimoWebScanEvents(ctx, stream, func(ev mimoWebEvent) bool {
		turn.apply(ev, nil)
		return turn.errText == ""
	})
	if err != nil {
		return proxy.UpstreamFailure(http.StatusBadGateway, "mimo-web: stream failed: "+err.Error())
	}
	if turn.errText != "" {
		return proxy.UpstreamFailure(http.StatusBadGateway, "mimo-web: "+turn.errText)
	}
	if turn.content.Len() == 0 && turn.reasoning.Len() == 0 {
		return proxy.UpstreamFailure(http.StatusBadGateway, "mimo-web: the Studio ended the stream without an answer")
	}
	message := map[string]any{
		"role":    "assistant",
		"content": turn.content.String(),
	}
	if turn.reasoning.Len() > 0 {
		message["reasoning_content"] = turn.reasoning.String()
	}
	return mimoWebWriteCompletion(w, model, responseID, turn, message, "stop")
}

// mimoWebWriteCompletion answers one buffered turn as a single
// chat.completion object.
func mimoWebWriteCompletion(w http.ResponseWriter, model, responseID string, turn *mimoWebTurn, message map[string]any, finish string) error {
	completion := map[string]any{
		"id":      responseID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
	}
	if usage := turn.usageMap(); usage != nil {
		completion["usage"] = usage
	}
	encoded, err := json.Marshal(completion)
	if err != nil {
		return fmt.Errorf("mimo-web: marshal completion: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(encoded)
	return err
}
