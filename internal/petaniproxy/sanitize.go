package petaniproxy

import (
	"math/rand/v2"
	"net/http"
	"strings"
)

// CommonUserAgents mirrors server.py's COMMON_USER_AGENTS rotation pool.
var CommonUserAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36 Edg/125.0.0.0",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
}

// blockedHeaders are always stripped (server.py's blocked_headers blocklist).
var blockedHeaders = map[string]bool{
	"x-forwarded-for":    true,
	"via":                true,
	"x-real-ip":          true,
	"cf-connecting-ip":   true,
	"x-proxy-user-agent": true,
}

// hopByHopHeaders are stripped per RFC 2616 §13.5.1. server.py only stripped
// the blockedHeaders above; the RFC set is applied here as the task requires.
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"proxy-connection":    true, // non-standard but hop-by-hop in practice
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// botUATokens marks a User-Agent as an obvious bot library, triggering rotation.
var botUATokens = []string{"python-requests", "aiohttp", "curl", "urllib", "httpx"}

func randIntN(n int) int {
	return rand.IntN(n)
}

func pickUserAgent(pool []string) string {
	if len(pool) == 0 {
		return ""
	}
	return pool[randIntN(len(pool))]
}

// Sanitizer rewrites client headers before they are forwarded upstream.
type Sanitizer struct {
	UserAgents []string
}

// NewSanitizer returns a Sanitizer seeded with a copy of CommonUserAgents.
func NewSanitizer() *Sanitizer {
	return &Sanitizer{UserAgents: append([]string(nil), CommonUserAgents...)}
}

// Sanitize returns a cleaned copy of h: hop-by-hop + blocked headers are
// removed (including any header named as a token in the Connection header),
// bot-like or missing User-Agents are replaced with a rotated one.
func (s *Sanitizer) Sanitize(h http.Header) http.Header {
	out := make(http.Header, len(h))

	// Headers listed as Connection tokens are hop-by-hop too.
	extra := make(map[string]bool)
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if t := strings.ToLower(strings.TrimSpace(tok)); t != "" {
				extra[t] = true
			}
		}
	}

	hasUA := false
	for k, vv := range h {
		kl := strings.ToLower(k)
		if blockedHeaders[kl] || hopByHopHeaders[kl] || extra[kl] {
			continue
		}
		if kl == "user-agent" {
			hasUA = true
			for _, v := range vv {
				out.Set(k, s.maybeRotateUA(v))
			}
			continue
		}
		for _, v := range vv {
			out.Add(k, v)
		}
	}
	if !hasUA {
		out.Set("User-Agent", pickUserAgent(s.UserAgents))
	}
	return out
}

// maybeRotateUA replaces bot-library or empty User-Agents with a random one
// from the pool, mirroring server.py's sanitize_headers.
func (s *Sanitizer) maybeRotateUA(ua string) string {
	if ua == "" {
		return pickUserAgent(s.UserAgents)
	}
	lower := strings.ToLower(ua)
	for _, tok := range botUATokens {
		if strings.Contains(lower, tok) {
			return pickUserAgent(s.UserAgents)
		}
	}
	return ua
}
