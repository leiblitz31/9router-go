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

// ForwardPoolside is ForwardOpenAI with the org-scoped model id fix.
func ForwardPoolside(w http.ResponseWriter, req *Request) error {
	if body, ok := setPoolsideModel(req.Body); ok {
		c := *req
		c.Body = body
		req = &c
	}
	return ForwardOpenAI(w, req)
}
