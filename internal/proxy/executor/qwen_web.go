package executor

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"time"

	"9router/proxy/internal/proxy"
)

// ForwardQwenWeb routes a chat completion through chat.qwen.ai's web API in
// the site's anonymous guest mode — no account, no credential, only the
// synthetic Baxia anti-bot tokens generated in qwen_web_baxia.go. The lane
// was verified live from this gateway's egress on 2026-11-02, twice: once
// with headers captured from a real browser and once with the fully
// synthetic generator this file ships.
//
// The turn is two upstream calls: open a chat (POST /api/v2/chats/new),
// then stream the completion (POST /api/v2/chat/completions?chat_id=…).
// The executor is stateless by design: every router request opens a fresh
// upstream chat and carries the whole OpenAI history as the messages array
// of that one completion, chained by fid/parentId the way the site's own
// client chains a thread. Nothing is pinned between turns, so there is no
// session to expire, leak or misattribute.
//
// v1 scope: text chat with thinking, streaming and non-streaming. Tool
// calling (a prompt-injected protocol on this surface, as on deepseek-web)
// and account/token mode are deliberately not in v1.
func ForwardQwenWeb(w http.ResponseWriter, req *Request) error {
	var envelope qwenWebRequestEnvelope
	if err := json.Unmarshal(req.Body, &envelope); err != nil {
		return fmt.Errorf("qw-web: parse request: %w", err)
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	model := envelope.Model
	if model == "" {
		model = req.ModelName
	}
	if !qwenWebModels[model] {
		return qwenWebFailure(http.StatusBadRequest, "qw-web: unknown model "+model)
	}
	if len(envelope.Messages) == 0 {
		return qwenWebFailure(http.StatusBadRequest, "qw-web received an empty messages array")
	}
	thinking := qwenWebThinkingEnabled(envelope)
	messages := qwenWebBuildMessages(envelope.Messages, model, thinking)
	if messages == nil {
		return qwenWebFailure(http.StatusBadRequest, "qw-web received a request with no text content")
	}

	return qwenWebExecute(w, req, ctx, qwenWebHTTPClient(), qwenWebCredentialFor(req), model, messages, thinking)
}

type qwenWebRequestEnvelope struct {
	Model           string            `json:"model"`
	Stream          bool              `json:"stream"`
	ReasoningEffort string            `json:"reasoning_effort"`
	EnableThinking  *bool             `json:"enable_thinking"`
	Messages        []qwenWebInputMsg `json:"messages"`
}

type qwenWebInputMsg struct {
	Role    string         `json:"role"`
	Content jsontext.Value `json:"content"`
}

// qwenWebModels is the v1 catalogue: the seven models the site's own picker
// offers a Free account (verified live 2026-11-02). Registration lives in
// providers' registry_models.go; this set is the executor's own guard so an
// unregistered id fails cleanly instead of reaching the upstream.
var qwenWebModels = map[string]bool{
	"qwen3.7-plus":       true,
	"qwen3.8-max":        true,
	"qwen3.8-omni-flash": true,
	"qwen3.7-max":        true,
	"qwen3.6-plus":       true,
	"qwen3.5-plus":       true,
	"qwen3.5-omni-plus":  true,
}

// qwenWebThinkingEnabled resolves the thinking switch the ds-web way: an
// explicit client signal wins, and the default follows the upstream's own
// behaviour — the site's client sends thinking_enabled:true with
// thinking_mode "Auto", so an unmarked request thinks.
func qwenWebThinkingEnabled(env qwenWebRequestEnvelope) bool {
	if env.EnableThinking != nil {
		return *env.EnableThinking
	}
	if env.ReasoningEffort != "" {
		return env.ReasoningEffort != "none"
	}
	return true
}

// qwenWebTextContent extracts the text of one OpenAI message content value:
// either a plain string or an array of content parts.
func qwenWebTextContent(raw jsontext.Value) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "input_text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// qwenWebWireMessage is one message in the completion body's messages
// array, shaped exactly as the site's client sends it (HAR capture,
// 2026-11-02).
type qwenWebWireMessage struct {
	ID            any            `json:"id"`
	Fid           string         `json:"fid"`
	ParentID      any            `json:"parentId"`
	ChildrenIDs   []string       `json:"childrenIds"`
	Role          string         `json:"role"`
	Content       string         `json:"content"`
	UserAction    string         `json:"user_action,omitempty"`
	Files         []any          `json:"files"`
	Timestamp     int64          `json:"timestamp"`
	Models        []string       `json:"models,omitempty"`
	Model         string         `json:"model"`
	ChatType      string         `json:"chat_type"`
	FeatureConfig *featureConfig `json:"feature_config,omitempty"`
	Extra         *extraMeta     `json:"extra,omitempty"`
	SubChatType   string         `json:"sub_chat_type"`
	ParentIDAlt   any            `json:"parent_id"`
}

type featureConfig struct {
	ThinkingEnabled bool   `json:"thinking_enabled"`
	OutputSchema    string `json:"output_schema"`
	ResearchMode    string `json:"research_mode"`
	AutoThinking    bool   `json:"auto_thinking"`
	ThinkingMode    string `json:"thinking_mode"`
	ThinkingFormat  string `json:"thinking_format"`
	AutoSearch      bool   `json:"auto_search"`
}

type extraMeta struct {
	Meta struct {
		SubChatType string `json:"subChatType"`
	} `json:"meta"`
}

// qwenWebBuildMessages converts the OpenAI history into the wire array.
//
// The array holds exactly ONE user message, always. That is a measured
// wire constraint, not a style choice (verified live 2026-11-02): the
// site's own client only ever sends one new message per completion — the
// thread lives server-side, chained by parent_id — and risk control
// refuses a completion whose messages carry any other shape this gateway
// tried: a "system" role message (RGV587) and a multi-message array with
// an "assistant" history entry (RGV587). The reference wrappers converge
// on the same answer: flatten the history into one string.
//
// So: a lone user message goes out verbatim, exactly as the web client
// would send it. Anything longer becomes a transcript — system text first
// as an unlabeled instruction block, then "User:"/"Assistant:" turns — in
// a single user message. The turn stays stateless (a fresh upstream chat
// per router request) and the model still sees the whole history.
func qwenWebBuildMessages(in []qwenWebInputMsg, model string, thinking bool) []qwenWebWireMessage {
	type turn struct{ role, text string }
	var turns []turn
	for _, m := range in {
		text := qwenWebTextContent(m.Content)
		if text == "" {
			continue
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		turns = append(turns, turn{role, text})
	}
	if len(turns) == 0 {
		return nil
	}
	content := turns[0].text
	if len(turns) > 1 || turns[0].role != "user" {
		var b strings.Builder
		for _, t := range turns {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			switch t.role {
			case "system", "developer":
				b.WriteString(t.text)
			case "assistant":
				b.WriteString("Assistant: " + t.text)
			default:
				b.WriteString("User: " + t.text)
			}
		}
		content = b.String()
	}
	fid := qwenWebUUID()
	msg := qwenWebWireMessage{
		Fid:         fid,
		Role:        "user",
		Content:     content,
		UserAction:  "chat",
		Files:       []any{},
		Timestamp:   time.Now().Unix(),
		Model:       "",
		ChatType:    "t2t",
		SubChatType: "t2t",
		ChildrenIDs: []string{qwenWebUUID()},
		Models:      []string{model},
	}
	fc := featureConfig{ThinkingEnabled: thinking, OutputSchema: "phase", ResearchMode: "normal",
		AutoThinking: true, ThinkingMode: "Auto", ThinkingFormat: "summary", AutoSearch: true}
	msg.FeatureConfig = &fc
	extra := extraMeta{}
	extra.Meta.SubChatType = "t2t"
	msg.Extra = &extra
	return []qwenWebWireMessage{msg}
}

// qwenWebSharedClient is the lane's own HTTP client. The router's shared
// client dials upstream hosts directly, and chat.qwen.ai's risk control
// refuses this gateway's direct egress — the lane only passes from the
// operator's configured proxy egress, which the reference wrappers reach
// through the standard proxy environment. A dedicated transport with
// ProxyFromEnvironment is therefore part of the lane's wire contract, the
// same reason deepseek-web builds its own transport. No client timeout:
// a completion stream is bounded by the request context, not the clock.
var qwenWebSharedClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	},
}

func qwenWebHTTPClient() *http.Client { return qwenWebSharedClient }

func qwenWebFailure(status int, message string) error {
	return proxy.UpstreamFailure(status, message)
}

// errQwenWebRiskControl marks a risk-control refusal so the turn runner can
// apply its single refresh-and-retry.
type qwenWebRiskControlError struct{ stage string }

func (e *qwenWebRiskControlError) Error() string {
	return "qw-web: risk control refused the " + e.stage + " request (RGV587)"
}
