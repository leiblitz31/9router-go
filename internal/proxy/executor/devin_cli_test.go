package executor

import (
	json "encoding/json/v2"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"9router/proxy/internal/providers"
)

// devinFakeBinary points CLI_DEVIN_BIN at the ACP stub and restores the
// environment afterwards, so the test never depends on a real Devin install.
func devinFakeBinary(t *testing.T) {
	t.Helper()
	abs, err := filepath.Abs("testdata/fake-devin-acp")
	if err != nil {
		t.Fatalf("resolve fake binary: %v", err)
	}
	if err := os.Chmod(abs, 0o755); err != nil {
		t.Fatalf("chmod fake binary: %v", err)
	}
	t.Setenv("CLI_DEVIN_BIN", abs)
	t.Setenv("CLI_DEVIN_AGENT_TYPE", "")
}

func devinTestRequest(t *testing.T, body string, stream bool) *Request {
	t.Helper()
	return &Request{
		Ctx:      t.Context(),
		Config:   &providers.ProviderConfig{ProviderID: "devin-cli", NoAuth: true},
		Body:     []byte(body),
		IsStream: stream,
	}
}

// The provider used to be wired to ForwardOpenAI, which POSTs to the pseudo-URL
// devin://acp/stdio and dies in dial. A turn must now round-trip the ACP
// handshake: initialize, session/new in a real cwd, session/prompt, permission
// auto-allow, streamed chunks, finish.
func TestForwardDevinCLI_StreamsACPTurn(t *testing.T) {
	devinFakeBinary(t)

	body := `{"model":"devin-cli/swe-1.6","messages":[
		{"role":"system","content":"be terse"},
		{"role":"user","content":[{"type":"text","text":"say hi"}]}]}`
	w := httptest.NewRecorder()
	if err := ForwardDevinCLI(w, devinTestRequest(t, body, true)); err != nil {
		t.Fatalf("ForwardDevinCLI: %v", err)
	}

	out := w.Body.String()
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	for _, want := range []string{`"role":"assistant"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream missing %q:\n%s", want, out)
		}
	}
	// Deltas split wherever the agent split them, so the assertion is on the
	// concatenation a client would build.
	answer := devinSSEText(t, out)
	if !strings.Contains(answer, "Hello, world") {
		t.Fatalf("assembled answer = %q, want the streamed chunks", answer)
	}
	// The stub prefixes its answer with the prompt length and only streams after
	// the permission request was answered, so this proves both that the
	// flattened prompt reached the agent and that the allow reply landed.
	if !strings.HasPrefix(answer, "echo[") || strings.HasPrefix(answer, "echo[0]") {
		t.Fatalf("prompt did not reach the agent: %q", answer)
	}
}

// stream:false must yield one chat.completion body with the concatenated text,
// not an event stream.
func TestForwardDevinCLI_NonStreamBody(t *testing.T) {
	devinFakeBinary(t)

	body := `{"model":"swe-1.6","messages":[{"role":"user","content":"say hi"}]}`
	w := httptest.NewRecorder()
	if err := ForwardDevinCLI(w, devinTestRequest(t, body, false)); err != nil {
		t.Fatalf("ForwardDevinCLI: %v", err)
	}

	out := w.Body.String()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if strings.Contains(out, "data: ") {
		t.Fatalf("non-stream response carries SSE:\n%s", out)
	}
	if !strings.Contains(out, `"content":"echo[`) || !strings.Contains(out, "world") {
		t.Fatalf("completion missing answer text:\n%s", out)
	}
	for _, want := range []string{`"object":"chat.completion"`, `"finish_reason":"stop"`, `"estimated":true`} {
		if !strings.Contains(out, want) {
			t.Fatalf("completion missing %q:\n%s", want, out)
		}
	}
}

// A bin that does not exist must name the install path instead of surfacing a
// bare fork/exec error to the caller.
func TestForwardDevinCLI_MissingBinaryExplainsInstall(t *testing.T) {
	t.Setenv("CLI_DEVIN_BIN", filepath.Join(t.TempDir(), "no-such-devin"))
	w := httptest.NewRecorder()
	err := ForwardDevinCLI(w, devinTestRequest(t, `{"model":"swe-1.6","messages":[{"role":"user","content":"hi"}]}`, true))
	if err == nil {
		t.Fatalf("expected an error for a missing binary")
	}
	if !strings.Contains(err.Error(), "https://cli.devin.ai") {
		t.Fatalf("error does not point at the installer: %v", err)
	}
}

// devinPrompt is the whole format negotiation: OpenAI and Claude shapes both
// collapse into one bracketed prompt.
func TestDevinPrompt_FlattensBothShapes(t *testing.T) {
	openAI := &devinBody{Messages: []devinMessage{
		{Role: "system", Content: []byte(`"be terse"`)},
		{Role: "user", Content: []byte(`[{"type":"text","text":"read a.txt"}]`)},
		{Role: "assistant", Content: []byte(`null`), ToolCalls: []devinToolCall{
			func() devinToolCall {
				var call devinToolCall
				call.ID = "call_1"
				call.Function.Name = "read_file"
				call.Function.Arguments = []byte(`{"path":"a.txt"}`)
				return call
			}(),
		}},
		{Role: "tool", ToolCallID: "call_1", Content: []byte(`"hello"`)},
	}}
	got := devinPrompt(openAI)
	for _, want := range []string{"[System]\nbe terse", "[User]\nread a.txt", "[Tool call read_file id=call_1]", "[Tool]\n[Tool result id=call_1]\nhello"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}

	claude := &devinBody{
		System: []byte(`[{"type":"text","text":"be terse"}]`),
		Messages: []devinMessage{{
			Role:    "user",
			Content: []byte(`[{"type":"text","text":"read a.txt"},{"type":"tool_result","tool_use_id":"toolu_1","content":"hello"}]`),
		}},
	}
	got = devinPrompt(claude)
	for _, want := range []string{"[System]\nbe terse", "read a.txt", "[Tool result id=toolu_1]\nhello"} {
		if !strings.Contains(got, want) {
			t.Fatalf("claude prompt missing %q:\n%s", want, got)
		}
	}
}

// A path that is not an existing absolute directory is not a workspace: the
// agent must never be spawned in a directory the gateway invented.
func TestDevinCWD_RejectsUnusableHints(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body *devinBody
		want string
	}{
		{name: "cwd field", body: &devinBody{CWD: dir}, want: dir},
		{name: "metadata", body: &devinBody{Metadata: struct {
			CWD              string `json:"cwd"`
			WorkingDirectory string `json:"working_directory"`
		}{CWD: dir}}, want: dir},
		{name: "relative rejected", body: &devinBody{CWD: "relative/path"}, want: os.TempDir()},
		{name: "missing rejected", body: &devinBody{CWD: filepath.Join(dir, "nope")}, want: os.TempDir()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if got := devinCWD(body, ""); got != tc.want {
				t.Fatalf("devinCWD = %q, want %q", got, tc.want)
			}
		})
	}

	if got := devinCWD(&devinBody{}, "<cwd>"+dir+"</cwd>"); got != dir {
		t.Fatalf("cwd tag not honoured: %q", got)
	}
}

// devinSSEText assembles the assistant text a streaming client would build from
// the recorder's event stream.
func devinSSEText(t *testing.T, sse string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(sse, "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad SSE frame %q: %v", data, err)
		}
		if len(chunk.Choices) > 0 {
			b.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	return b.String()
}

// The real CLI (devin 3000.11.92) stops an unauthenticated session with cause
// "auth_required" and streams nothing. That must be an error naming the login,
// not an empty 200 completion: the upstream reference only fails on
// cause=="error", which is how a signed-out host ends up serving blank answers.
func TestForwardDevinCLI_UnauthenticatedStopIsAnError(t *testing.T) {
	devinFakeBinary(t)
	t.Setenv("FAKE_DEVIN_MODE", "auth_required")

	w := httptest.NewRecorder()
	err := ForwardDevinCLI(w, devinTestRequest(t, `{"model":"swe-1.6","messages":[{"role":"user","content":"hi"}]}`, true))
	if err == nil {
		t.Fatalf("an auth_required stop produced no error (body: %s)", w.Body.String())
	}
	if !strings.Contains(err.Error(), "auth") || !strings.Contains(err.Error(), "devin auth login") {
		t.Fatalf("error does not explain how to authenticate: %v", err)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("nothing should be written before the turn fails, got: %s", w.Body.String())
	}
}
