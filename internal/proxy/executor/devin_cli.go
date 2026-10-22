package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"9router/proxy/internal/handlerutil"
)

// Devin CLI is not an HTTP provider. Its registry entry carries the pseudo-URL
// devin://acp/stdio because everything travels over ACP (Agent Client Protocol):
// JSON-RPC 2.0 on the stdin/stdout of `devin acp`. This executor spawns that
// process, drives the handshake the CLI expects — initialize → session/new →
// session/prompt — answers permission requests, and re-emits the streamed
// session/update notifications as OpenAI SSE, so an OpenAI client never learns
// that the "upstream" is a subprocess.
//
// Port of upstream open-sse/executors devin-cli (class extends BaseExecutor,
// baseUrl devin://acp/stdio).
//
// ponytail: no client-tools MCP bridge. The upstream writes a Node .mjs shim and
// hands it to the agent as one MCP server so the caller's `tools` becomes
// callable inside the agent; here the tools are flattened into the prompt text
// instead. Ceiling: Devin answers but never calls the caller's tools. Add when a
// gateway truly has to route tool execution through Devin.

const (
	// devinProtocolVersion is the ACP revision the CLI advertises support for.
	devinProtocolVersion = "0.3"

	// devinMaxFrame bounds one JSON-RPC line. session/update carries file diffs
	// and tool output, so the 64k default scanner token is far too small.
	devinMaxFrame = 8 << 20

	// devinKillGrace is how long a finished session gets to exit on its own
	// after stdin closes, before it is killed.
	devinKillGrace = 2 * time.Second
)

// devinCWDRE picks a workspace path out of prompt text, matching the upstream
// hint scan: clients that cannot set a body field write <cwd>/path</cwd>.
var devinCWDRE = regexp.MustCompile(`(?i)<cwd>\s*([^<]+?)\s*</cwd>`)

type devinContentPart struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Name      string         `json:"name"`
	ID        string         `json:"id"`
	Input     jsontext.Value `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
	Content   jsontext.Value `json:"content"`
}

type devinToolCall struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Function struct {
		Name      string         `json:"name"`
		Arguments jsontext.Value `json:"arguments"`
	} `json:"function"`
}

type devinMessage struct {
	Role       string          `json:"role"`
	Content    jsontext.Value  `json:"content"`
	ToolCalls  []devinToolCall `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// devinBody is the request as the CLI needs to see it: one prompt string plus a
// working directory. Sampling params and tool schemas have no ACP-side
// equivalent and are dropped.
type devinBody struct {
	Model     string         `json:"model"`
	System    jsontext.Value `json:"system"`
	Messages  []devinMessage `json:"messages"`
	Input     []devinMessage `json:"input"`
	CWD       string         `json:"cwd"`
	WorkDir   string         `json:"working_directory"`
	Workdir   string         `json:"workdir"`
	Workspace string         `json:"workspace"`
	Metadata  struct {
		CWD              string `json:"cwd"`
		WorkingDirectory string `json:"working_directory"`
	} `json:"metadata"`
}

// devinFrame is one JSON-RPC line from the CLI. ID stays raw so a reply to the
// CLI's own request (session/request_permission) echoes the id byte for byte
// instead of round-tripping it through a float.
type devinFrame struct {
	ID     jsontext.Value `json:"id"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params"`
	Result jsontext.Value `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ForwardDevinCLI serves the devin-cli provider over ACP/stdio.
func ForwardDevinCLI(w http.ResponseWriter, req *Request) error {
	var body devinBody
	if err := json.Unmarshal(req.Body, &body, handlerutil.ClientBody); err != nil {
		return fmt.Errorf("devin-cli: parse body: %w", err)
	}

	model := strings.TrimSpace(body.Model)
	if model == "" {
		model = req.ModelName
	}
	model = stripOpenCodePrefix(model, "devin-cli", "dv")

	prompt := devinPrompt(&body)
	cwd := devinCWD(&body, prompt)

	bin := devinBin()
	args := []string{"acp"}
	if agentType := strings.TrimSpace(os.Getenv("CLI_DEVIN_AGENT_TYPE")); agentType != "" {
		args = append(args, "--agent-type", agentType)
	}

	ctx, cancel := context.WithCancel(reqCtx(req))
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "DEVIN_PERMISSION_MODE="+devinPermissionMode())

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("devin-cli: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("devin-cli: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("devin-cli: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("devin-cli: Devin CLI not found at %q — install it from https://cli.devin.ai or point CLI_DEVIN_BIN at the binary: %w", bin, err)
		}
		return fmt.Errorf("devin-cli: spawn %q: %w", bin, err)
	}

	var stderrTail strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		buf := make([]byte, 4096)
		for {
			n, rerr := stderr.Read(buf)
			if n > 0 {
				stderrTail.Write(buf[:n])
				if s := stderrTail.String(); len(s) > 4096 {
					stderrTail.Reset()
					stderrTail.WriteString(s[len(s)-2048:])
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Close stdin first: a drained ACP session exits on its own, and only a
	// stuck one needs the SIGKILL. Waiting before killing keeps the grace period
	// from becoming a guaranteed 2s tail on every turn.
	stop := func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(devinKillGrace):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		}
		<-stderrDone
	}
	defer stop()

	stream := &devinStream{
		w:       w,
		model:   model,
		created: time.Now().Unix(),
		sse:     req.IsStream,
		prompt:  prompt,
		buf:     req.ResponseBuf,
	}
	result := devinResult{}
	driver := devinDriver{
		stdin:  stdin,
		stream: stream,
		model:  model,
		cwd:    cwd,
		prompt: prompt,
		result: &result,
		stderr: &stderrTail,
		mu:     &sync.Mutex{},
	}

	if err := driver.run(stdout); err != nil {
		if stream.started {
			// Headers are already out; the only honest way to fail a live
			// stream is an in-band error frame the client will surface.
			stream.emitError(err.Error())
			return nil
		}
		return err
	}

	// A turn that streamed nothing still owes the client its answer.
	if stream.text.Len() == 0 && result.text != "" {
		stream.textChunk(result.text)
	}
	if req.IsStream {
		stream.finish(result.stopReason)
		return nil
	}
	return stream.writeFull(result.stopReason)
}

// devinResult is what the session/prompt response said, used when the agent
// never streamed its text (some turns answer only in the result body).
type devinResult struct {
	text       string
	stopReason string
}

type devinDriver struct {
	stdin  io.Writer
	stream *devinStream
	model  string
	cwd    string
	prompt string
	result *devinResult
	stderr *strings.Builder
	mu     *sync.Mutex
	nextID int
}

// run drives the ACP conversation to completion. Every outbound request is a
// numbered JSON-RPC request and responses are matched by id, not by arrival
// order: the CLI interleaves notifications and permission prompts between them.
func (d *devinDriver) run(stdout io.Reader) error {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), devinMaxFrame)

	initID, err := d.send("initialize", map[string]any{
		"protocolVersion": devinProtocolVersion,
		"clientInfo":      map[string]any{"name": "9router", "version": "1.0"},
		"capabilities":    map[string]any{},
	})
	if err != nil {
		return fmt.Errorf("devin-cli: write initialize: %w", err)
	}
	newID, promptID := 0, 0

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame devinFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			continue // non-JSON chatter on stdout is not ours to fail on
		}

		switch {
		case frame.Error != nil:
			return fmt.Errorf("Devin ACP error %d: %s", frame.Error.Code, frame.Error.Message)

		case frame.Method == "session/request_permission" && len(frame.ID) > 0:
			d.allowPermission(frame)

		case frame.Method == "_cognition.ai/agent_stopped" || frame.Method == "$/agent_stopped":
			// cause carries the real outcome: an unauthenticated CLI stops with
			// "auth_required" and never streams a byte, so treating every
			// non-"error" stop as success serves an empty 200 and hides the
			// reason. Only the benign causes end the turn cleanly.
			cause := devinStringField(frame.Params, "cause")
			switch cause {
			case "", "end_turn", "stop", "completed", "success":
				return nil
			}
			msg := devinStringField(frame.Params, "errorMessage")
			if msg == "" {
				msg = devinStringField(frame.Params, "message")
			}
			if msg == "" {
				msg = devinStringField(frame.Params, "error")
			}
			if msg == "" {
				msg = "agent stopped: " + cause
			}
			if cause == "auth_required" {
				msg += " (run `devin auth login` on the host running 9router)"
			}
			return fmt.Errorf("devin-cli: %s", msg)

		case frame.Method == "session/update" || frame.Method == "$/update":
			done, uerr := d.handleUpdate(frame)
			if uerr != nil {
				return uerr
			}
			if done {
				return nil
			}

		case len(frame.ID) > 0 && frame.Method == "" && len(frame.Result) > 0:
			id, _ := strconv.Atoi(string(frame.ID))
			switch id {
			case initID:
				if newID, err = d.send("session/new", map[string]any{
					"cwd":        d.cwd,
					"mcpServers": []any{},
					"model":      d.model,
				}); err != nil {
					return fmt.Errorf("devin-cli: write session/new: %w", err)
				}
			case newID:
				sessionID := devinStringField(frame.Result, "sessionId")
				if sessionID == "" {
					return fmt.Errorf("devin-cli: session/new returned no sessionId")
				}
				if promptID, err = d.send("session/prompt", map[string]any{
					"sessionId": sessionID,
					"prompt":    []any{map[string]any{"type": "text", "text": d.prompt}},
				}); err != nil {
					return fmt.Errorf("devin-cli: write session/prompt: %w", err)
				}
			case promptID:
				d.readPromptResult(frame.Result)
				return nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("devin-cli: read stdout: %w", err)
	}
	tail := strings.TrimSpace(d.stderr.String())
	if tail == "" {
		return fmt.Errorf("devin-cli: Devin CLI exited before the turn completed")
	}
	return fmt.Errorf("devin-cli: Devin CLI exited before the turn completed: %s", devinTruncate(tail, 400))
}

// send writes one JSON-RPC request and returns its id.
func (d *devinDriver) send(method string, params any) (int, error) {
	d.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": d.nextID, "method": method, "params": params}
	payload, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err = d.stdin.Write(append(payload, '\n'))
	return d.nextID, err
}

// allowPermission answers the CLI's permission prompt. There is no human on this
// side of a gateway request, so the first "allow" option wins — the same choice
// the upstream makes — and DEVIN_PERMISSION_MODE=bypass keeps the prompt rare.
func (d *devinDriver) allowPermission(frame devinFrame) {
	var params struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(frame.Params, &params); err != nil || len(params.Options) == 0 {
		return
	}
	chosen := params.Options[0].OptionID
	for _, opt := range params.Options {
		if strings.Contains(strings.ToLower(opt.Kind), "allow") {
			chosen = opt.OptionID
			break
		}
	}
	if chosen == "" {
		return
	}
	reply, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      jsontext.Value(frame.ID),
		"result": map[string]any{
			"outcome": map[string]any{"outcome": "selected", "optionId": chosen},
		},
	})
	if err != nil {
		return
	}
	d.mu.Lock()
	_, _ = d.stdin.Write(append(reply, '\n'))
	d.mu.Unlock()
}

// handleUpdate maps one session/update notification onto the client stream.
// done=true means the turn ended.
func (d *devinDriver) handleUpdate(frame devinFrame) (bool, error) {
	var params struct {
		Type   string `json:"type"`
		Delta  string `json:"delta"`
		Text   string `json:"text"`
		Update struct {
			SessionUpdate string         `json:"sessionUpdate"`
			Content       jsontext.Value `json:"content"`
		} `json:"update"`
	}
	if err := json.Unmarshal(frame.Params, &params); err != nil {
		return false, nil
	}
	kind := params.Update.SessionUpdate
	if kind == "" {
		kind = params.Type
	}

	switch kind {
	case "agent_message_chunk", "message_delta", "text_delta", "content_delta":
		text := devinChunkText(params.Update.Content)
		if text == "" {
			text = params.Delta
		}
		if text == "" {
			text = params.Text
		}
		if text != "" {
			d.stream.textChunk(text)
		}
	case "agent_thought_chunk":
		// Reasoning stays out of the answer: an OpenAI client renders deltas as
		// assistant text, and a thought headline would land in the transcript.
	case "message_stop", "stop", "done":
		return true, nil
	case "error":
		msg := devinStringField(frame.Params, "message")
		if msg == "" {
			msg = "Devin ACP error"
		}
		return true, fmt.Errorf("devin-cli: %s", msg)
	}
	return false, nil
}

// readPromptResult captures the final answer for the case where nothing was
// streamed, and the stop reason for the finish chunk.
func (d *devinDriver) readPromptResult(raw jsontext.Value) {
	var result struct {
		StopReason string         `json:"stopReason"`
		Content    jsontext.Value `json:"content"`
		Text       string         `json:"text"`
		Message    struct {
			Content jsontext.Value `json:"content"`
		} `json:"message"`
		Messages []struct {
			Role    string         `json:"role"`
			Content jsontext.Value `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return
	}
	d.result.stopReason = result.StopReason

	text := devinChunkText(result.Content)
	if text == "" {
		text = result.Text
	}
	if text == "" {
		text = devinChunkText(result.Message.Content)
	}
	if text == "" {
		var joined []string
		for _, msg := range result.Messages {
			if msg.Role == "assistant" {
				if part := devinChunkText(msg.Content); part != "" {
					joined = append(joined, part)
				}
			}
		}
		text = strings.Join(joined, "\n")
	}
	d.result.text = strings.TrimSpace(text)
}

// devinStream renders the ACP output as an OpenAI completion. Streaming and
// buffered mode share the text accumulator, so a stream:false client gets the
// exact bytes a streaming one would have concatenated.
type devinStream struct {
	w       http.ResponseWriter
	buf     io.Writer // handler-side capture for token estimation/logging
	model   string
	prompt  string
	id      string
	created int64
	sse     bool
	started bool
	text    strings.Builder
	flusher http.Flusher
}

func (s *devinStream) begin() {
	if s.started {
		return
	}
	s.started = true
	s.id = "chatcmpl-devin-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if !s.sse {
		return
	}
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.WriteHeader(http.StatusOK)
	s.flusher, _ = s.w.(http.Flusher)
	_ = writeSSE(s.w, map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}},
	})
	s.flush()
}

func (s *devinStream) textChunk(text string) {
	s.begin()
	s.text.WriteString(text)
	if !s.sse {
		return
	}
	_ = writeSSE(s.w, map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
	})
	s.flush()
}

// finish closes a streamed answer; the buffered path is closed by writeFull.
func (s *devinStream) finish(stopReason string) {
	s.begin()
	if s.buf != nil {
		_, _ = io.WriteString(s.buf, s.text.String())
	}
	_ = writeSSE(s.w, map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": devinFinishReason(stopReason)}},
		"usage":   s.usage(),
	})
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	s.flush()
}

// emitError reports a failure that happened after the headers went out. The
// client sees the error object in-band, the same shape the upstream emits.
func (s *devinStream) emitError(msg string) {
	s.begin()
	_ = writeSSE(s.w, map[string]any{"error": map[string]any{"message": msg, "type": "devin_cli_error"}})
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	s.flush()
}

// writeFull serves a stream:false request with one chat.completion body.
func (s *devinStream) writeFull(stopReason string) error {
	s.begin()
	if s.buf != nil {
		_, _ = io.WriteString(s.buf, s.text.String())
	}
	body, err := json.Marshal(map[string]any{
		"id": s.id, "object": "chat.completion", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": s.text.String()},
			"finish_reason": devinFinishReason(stopReason),
		}},
		"usage": s.usage(),
	})
	if err != nil {
		return fmt.Errorf("devin-cli: marshal completion: %w", err)
	}
	s.w.Header().Set("Content-Type", "application/json")
	s.w.WriteHeader(http.StatusOK)
	_, err = s.w.Write(body)
	return err
}

// usage is the character-quarter estimate the upstream reports: ACP carries no
// token counts, and a zeroed usage makes every client think the turn was free.
func (s *devinStream) usage() map[string]any {
	promptTokens := len(s.prompt) / 4
	completionTokens := len(s.text.String()) / 4
	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
		"estimated":         true,
	}
}

func (s *devinStream) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// devinFinishReason maps an ACP stopReason onto the OpenAI vocabulary.
func devinFinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens", "max_output_tokens", "refusal":
		return "length"
	default:
		return "stop"
	}
}

// devinPrompt flattens an OpenAI (or Claude) message list into the single text
// prompt ACP accepts. Tool calls and results ride along as bracketed lines so a
// multi-turn agentic conversation survives the format loss.
func devinPrompt(body *devinBody) string {
	var out []string
	appendMsg := func(role, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		switch role {
		case "system":
			out = append(out, "[System]\n"+text)
		case "assistant":
			out = append(out, "[Assistant]\n"+text)
		case "tool":
			out = append(out, "[Tool]\n"+text)
		default:
			out = append(out, "[User]\n"+text)
		}
	}

	if system := devinChunkText(body.System); strings.TrimSpace(system) != "" {
		appendMsg("system", system)
	}
	messages := body.Messages
	if len(messages) == 0 {
		messages = body.Input
	}
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		text := devinContentText(msg.Content)
		if role == "assistant" && len(msg.ToolCalls) > 0 {
			parts := []string{text}
			for _, call := range msg.ToolCalls {
				name := call.Function.Name
				if name == "" {
					name = call.Name
				}
				args := "{}"
				if len(call.Function.Arguments) > 0 {
					args = string(call.Function.Arguments)
				}
				parts = append(parts, fmt.Sprintf("[Tool call %s id=%s]\n%s", name, call.ID, args))
			}
			text = strings.Join(devinDropEmpty(parts), "\n\n")
		}
		if role == "tool" {
			text = fmt.Sprintf("[Tool result id=%s]\n%s", msg.ToolCallID, text)
		}
		appendMsg(role, text)
	}
	if len(out) == 0 {
		return "(empty)"
	}
	return strings.Join(out, "\n\n")
}

// devinContentText reads a message content that is either a plain string or an
// array of typed parts.
func devinContentText(raw jsontext.Value) string {
	if len(raw) == 0 {
		return ""
	}
	if raw.Kind() == '"' {
		return devinChunkText(raw)
	}
	if raw.Kind() != '[' {
		return ""
	}
	var parts []devinContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		var scalars []string
		if err := json.Unmarshal(raw, &scalars); err == nil {
			return strings.Join(scalars, "\n")
		}
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text":
			b.WriteString(part.Text)
		case "tool_use":
			input := "{}"
			if len(part.Input) > 0 {
				input = string(part.Input)
			}
			fmt.Fprintf(&b, "\n[Tool call %s id=%s]\n%s\n", part.Name, part.ID, input)
		case "tool_result":
			fmt.Fprintf(&b, "\n[Tool result id=%s]\n%s\n", part.ToolUseID, devinChunkText(part.Content))
		}
	}
	return b.String()
}

// devinChunkText pulls text out of an ACP content value, which is either a
// string or {type:"text",text:"…"}.
func devinChunkText(raw jsontext.Value) string {
	if len(raw) == 0 {
		return ""
	}
	if raw.Kind() == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	if raw.Kind() == '{' {
		var part devinContentPart
		if err := json.Unmarshal(raw, &part); err == nil {
			if part.Text != "" {
				return part.Text
			}
			if len(part.Content) > 0 {
				return devinChunkText(part.Content)
			}
		}
	}
	// A Claude system prompt arrives as an array of blocks, which is the same
	// shape as message content.
	if raw.Kind() == '[' {
		return devinContentText(raw)
	}
	return ""
}

// devinStringField reads one string member out of a raw object.
func devinStringField(raw jsontext.Value, key string) string {
	if len(raw) == 0 || raw.Kind() != '{' {
		return ""
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	value, ok := fields[key]
	if !ok || value.Kind() != '"' {
		return ""
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return ""
	}
	return s
}

// devinCWD picks the agent's working directory: a body field or a <cwd> hint in
// the prompt, accepted only when it is an absolute path to an existing
// directory, else the system temp dir. The ACP session is created in a real
// directory or not at all.
func devinCWD(body *devinBody, prompt string) string {
	var candidates []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			candidates = append(candidates, s)
		}
	}
	add(body.CWD)
	add(body.WorkDir)
	add(body.Workdir)
	add(body.Workspace)
	add(body.Metadata.CWD)
	add(body.Metadata.WorkingDirectory)
	for _, match := range devinCWDRE.FindAllStringSubmatch(prompt, -1) {
		add(match[1])
	}
	for _, c := range candidates {
		if !filepath.IsAbs(c) {
			continue
		}
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return os.TempDir()
}

// devinBin resolves the CLI: an explicit CLI_DEVIN_BIN, the installer's known
// locations, then PATH.
func devinBin() string {
	if bin := strings.TrimSpace(os.Getenv("CLI_DEVIN_BIN")); bin != "" {
		return bin
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "share", "devin", "bin", "devin"),
		filepath.Join(home, ".devin", "bin", "devin"),
		filepath.Join(home, ".local", "bin", "devin"),
		"/opt/homebrew/bin/devin",
		"/usr/local/bin/devin",
		"/usr/bin/devin",
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	if path, err := exec.LookPath("devin"); err == nil {
		return path
	}
	return "devin"
}

// devinPermissionMode is the CLI's own permission policy. A gateway has nobody
// to approve a tool call, so bypass is the default and a deployment can still
// tighten it through the environment.
func devinPermissionMode() string {
	if mode := strings.TrimSpace(os.Getenv("DEVIN_PERMISSION_MODE")); mode != "" {
		return mode
	}
	return "bypass"
}

func devinDropEmpty(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func devinTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
