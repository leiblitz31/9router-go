package executor

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Workers AI's OpenAI-compatible chat endpoint accepts at most one system
// message, and it must be the first message. A second system message —
// even immediately after the first — is answered with 400 code 8007
// "System message must be at the beginning." Hermes (and other OpenAI-shaped
// clients) routinely send two leading system messages, so a verbatim
// passthrough makes every such session fail on cloudflare-ai while the
// same payload works on providers that tolerate multiple systems.
//
// This executor is therefore cloudflare-ai only: it merges every
// role:"system" message into a single leading system message, in order,
// joined with a blank line, and leaves every other message — and every
// other field — untouched. A body that already carries exactly one system
// message at index 0 is passed through byte-identical.
func ForwardCloudflareAI(w http.ResponseWriter, req *Request) error {
	if body, ok := mergeCloudflareSystemMessages(req.Body); ok {
		c := *req
		c.Body = body
		req = &c
	}
	return ForwardOpenAI(w, req)
}

// mergeCloudflareSystemMessages returns the body with all system messages
// merged into one leading message, and true when it changed anything.
// It returns the body unchanged and false when there is nothing to merge,
// when the body is not a JSON object with a messages array, or when a
// system message carries content in a shape it cannot merge losslessly
// (anything other than a string or a text-block array): in that last case
// the upstream is the authority on rejecting the request, and silently
// dropping an instruction would be worse than forwarding it.
func mergeCloudflareSystemMessages(body []byte) ([]byte, bool) {
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		return body, false
	}
	msgs, ok := env["messages"].([]any)
	if !ok {
		return body, false
	}

	var systemTexts []string
	var systemCount int
	var rest []any
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "system" {
			rest = append(rest, m)
			continue
		}
		systemCount++
		text, ok := cloudflareSystemText(msg["content"])
		if !ok {
			return body, false
		}
		if text != "" {
			systemTexts = append(systemTexts, text)
		}
	}

	// No system message at all: nothing to do.
	if systemCount == 0 {
		return body, false
	}

	// Exactly one non-empty system message, already first: nothing to do.
	// (An empty system message is dropped by the merge below, which is a
	// change, so it does not take this fast path.)
	if len(systemTexts) == 1 && len(rest) == len(msgs)-1 {
		if first, ok := msgs[0].(map[string]any); ok && first["role"] == "system" {
			return body, false
		}
	}

	out := make([]any, 0, len(rest)+1)
	if len(systemTexts) > 0 {
		out = append(out, map[string]any{
			"role":    "system",
			"content": strings.Join(systemTexts, "\n\n"),
		})
	}
	out = append(out, rest...)
	env["messages"] = out

	merged, err := json.Marshal(env)
	if err != nil {
		return body, false
	}
	return merged, true
}

// cloudflareSystemText extracts the text of a system message's content:
// a plain string, or a block array whose text blocks are concatenated.
// The second return value is false for any other shape.
func cloudflareSystemText(content any) (string, bool) {
	switch c := content.(type) {
	case string:
		return c, true
	case []any:
		var parts []string
		for _, b := range c {
			block, ok := b.(map[string]any)
			if !ok {
				return "", false
			}
			text, _ := block["text"].(string)
			if text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, ""), true
	case nil:
		return "", true
	default:
		return "", false
	}
}
