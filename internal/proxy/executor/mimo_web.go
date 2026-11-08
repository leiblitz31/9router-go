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

	responseID := "chatcmpl-mimo-" + mimoWebRandomID()[:12]
	if req.IsStream {
		return mimoWebStreamToClient(ctx, w, req, stream, model, responseID, tools)
	}
	return mimoWebCollectToClient(ctx, w, req, stream, model, responseID, tools)
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
func mimoWebStreamToClient(ctx context.Context, w http.ResponseWriter, req *Request, stream io.ReadCloser, model, responseID string, tools []deepSeekToolSpec) error {
	if len(tools) > 0 {
		return mimoWebStreamToolTurn(ctx, w, req, stream, model, responseID, tools)
	}
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

// mimoWebStreamToolTurn answers a streaming request that declared
// tools. Unlike the plain path it buffers the whole turn before writing
// anything: a call written as JSON or DSML markup cannot be recognised
// — or kept out of the client's content channel — until the answer is
// complete, and an agent client waits for the call rather than reading
// prose token by token, so nothing it values is lost. Reasoning and
// content are then emitted as single deltas ahead of the finish chunk.
func mimoWebStreamToolTurn(ctx context.Context, w http.ResponseWriter, req *Request, stream io.ReadCloser, model, responseID string, tools []deepSeekToolSpec) error {
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
	if req.TTFT != nil && *req.TTFT == 0 && !req.StartTime.IsZero() {
		*req.TTFT = time.Since(req.StartTime).Milliseconds()
	}
	message, finish := mimoWebFinalizeMessage(turn, tools)
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

// mimoWebCollectToClient buffers the turn and answers one completion.
func mimoWebCollectToClient(ctx context.Context, w http.ResponseWriter, req *Request, stream io.ReadCloser, model, responseID string, tools []deepSeekToolSpec) error {
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
	var message map[string]any
	finish := "stop"
	if len(tools) > 0 {
		message, finish = mimoWebFinalizeMessage(turn, tools)
	} else {
		message = map[string]any{
			"role":    "assistant",
			"content": turn.content.String(),
		}
		if turn.reasoning.Len() > 0 {
			message["reasoning_content"] = turn.reasoning.String()
		}
	}
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
