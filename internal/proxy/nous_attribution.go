package proxy

import (
	"net/url"
	"slices"
	"strings"

	json "encoding/json/v2"
)

// Nous Portal requires attribution on some API-key models. A neutral router
// identity is used rather than exposing a credential or claiming another
// client's name (upstream NOUS_ATTRIBUTION, decolua/9router 90f89471 / #4770).
var nousAttributionTags = []string{"product=9router", "client=9router", "user=9router"}

// NousAttributionShape identifies the exact upstream this rule targets.
const (
	nousAttributionHost = "inference-api.nousresearch.com"
	nousAttributionPath = "/v1/chat/completions"
)

// ApplyNousAttribution adds the attribution tags to a body bound for the Nous
// Portal Chat Completions endpoint.
//
// Rules, all from upstream applyNousAttribution:
//
//   - only a custom `openai-compatible-` node (a bare `nous` provider config
//     goes through its own executor);
//   - only an API-key request — no key, no attribution;
//   - only https + that exact host + that exact path;
//   - the caller's own tags survive, and only blank values are replaced: an
//     empty `product=` does not satisfy the requirement upstream, so the
//     fallback is appended while a real caller value is kept.
func ApplyNousAttribution(providerID string, apiKey string, baseURL string, body []byte) []byte {
	if apiKey == "" || !strings.HasPrefix(providerID, "openai-compatible-") {
		return body
	}
	if !isNousChatCompletions(baseURL) {
		return body
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		return body
	}
	if tags, present := req["tags"]; present {
		// A non-array `tags` is not ours to rewrite.
		if _, isSlice := tags.([]any); !isSlice {
			return body
		}
	}

	tags := make([]string, 0, len(nousAttributionTags)+4)
	if existing, ok := req["tags"].([]any); ok {
		for _, t := range existing {
			if s, isStr := t.(string); isStr {
				tags = append(tags, s)
			}
		}
	}
	for _, fallback := range nousAttributionTags {
		prefix := fallback[:strings.Index(fallback, "=")+1]
		// Drop every value for this key, then keep one only if it was real:
		// an empty value does not satisfy the requirement upstream, a caller's
		// own non-blank value wins.
		kept := make([]string, 0, len(tags)+1)
		hasRealValue := false
		for _, tag := range tags {
			if strings.HasPrefix(tag, prefix) {
				if strings.TrimSpace(strings.TrimPrefix(tag, prefix)) != "" {
					kept = append(kept, tag)
					hasRealValue = true
				}
				continue
			}
			kept = append(kept, tag)
		}
		tags = kept
		if !hasRealValue {
			tags = append(tags, fallback)
		}
	}

	// Dedupe while keeping first-seen order.
	out := tags[:0]
	for _, tag := range tags {
		if !slices.Contains(out, tag) {
			out = append(out, tag)
		}
	}
	asAny := make([]any, 0, len(out))
	for _, tag := range out {
		asAny = append(asAny, tag)
	}
	req["tags"] = asAny

	patched, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return patched
}

// isNousChatCompletions reports whether baseURL is exactly the Nous Portal
// Chat Completions endpoint over https.
func isNousChatCompletions(baseURL string) bool {
	if baseURL == "" {
		return false
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Hostname() == nousAttributionHost && u.Path == nousAttributionPath
}
