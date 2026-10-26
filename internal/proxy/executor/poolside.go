package executor

import (
	"encoding/json"
	"net/http"
	"strings"
)

// poolsideUpstreamID re-scopes a bare model id to the org-scoped form
// inference.poolside.ai requires. The router serves "ps/laguna-s-2.1" and
// resolves it to the bare "laguna-s-2.1", but Poolside only accepts
// "poolside/<model>" — a bare id 404s with "please check the model you
// provided". Ids that already carry a slash (the double-prefixed served
// form "ps/poolside/laguna-s-2.1") pass through untouched.
func poolsideUpstreamID(model string) string {
	if model == "" || strings.Contains(model, "/") {
		return model
	}
	return "poolside/" + model
}

// setPoolsideModel rewrites the body's model field to the upstream form.
// It reports false when nothing changed (or the body is not a JSON object
// with a string model), leaving the caller's bytes untouched.
func setPoolsideModel(body []byte) ([]byte, bool) {
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false
	}
	m, _ := env["model"].(string)
	fixed := poolsideUpstreamID(m)
	if fixed == m {
		return nil, false
	}
	env["model"] = fixed
	out, err := json.Marshal(env)
	if err != nil {
		return nil, false
	}
	return out, true
}

// stripPoolsideReasoningCollision drops the `reasoning` key from any message
// that also carries `reasoning_content`. Poolside's upstream deserializer
// maps `reasoning` onto the `reasoning_content` field, so a message carrying
// both — an OpenAI client echoing back streamed reasoning_content alongside
// its own `reasoning` field, as Hermes does on later turns of a session —
// is rejected with HTTP 400 "duplicate field `reasoning_content`".
// Messages with only one of the two keys pass through untouched, so
// currently-working payloads are byte-identical.
func stripPoolsideReasoningCollision(body []byte) ([]byte, bool) {
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false
	}
	msgs, ok := env["messages"].([]any)
	if !ok {
		return nil, false
	}
	changed := false
	for _, msg := range msgs {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		if _, hasRC := m["reasoning_content"]; hasRC {
			if _, hasR := m["reasoning"]; hasR {
				delete(m, "reasoning")
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, false
	}
	return out, true
}

// ForwardPoolside is ForwardOpenAI with the org-scoped model id fix.
func ForwardPoolside(w http.ResponseWriter, req *Request) error {
	changed := false
	body := req.Body
	if fixed, ok := setPoolsideModel(body); ok {
		body, changed = fixed, true
	}
	if stripped, ok := stripPoolsideReasoningCollision(body); ok {
		body, changed = stripped, true
	}
	if changed {
		c := *req
		c.Body = body
		req = &c
	}
	return ForwardOpenAI(w, req)
}
