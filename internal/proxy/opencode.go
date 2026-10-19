package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultOpenCodeUA is the official OpenCode fingerprint User-Agent.
	// OpenCode v2 fingerprint required by the free-tier /inference/v1 API.
	// Bumped to the current CLI release (azrinaldi/opencode-free-bridge
	// derives this from the update API instead of hardcoding it).
	DefaultOpenCodeUA = "opencode/latest/2.0.32/cli"

	// DefaultOpenCodeV1UA is the pre-v2 identity kept by the keyed lanes
	// (opencode-zen, opencode-go): upstream decolua/9router#4842 moves only
	// the keyless free executor to v2 and leaves every keyed executor on v1.
	DefaultOpenCodeV1UA = "opencode/1.18.31"

	base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// OpenCodeSessionRegex matches canonical OpenCode session IDs: ses_ + 12 hex + 14 Base62 (30 chars).
var OpenCodeSessionRegex = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// OpenCodeRequestRegex matches canonical OpenCode request IDs: msg_ + 12 hex + 14 Base62 (30 chars).
var OpenCodeRequestRegex = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func unstableRandomBase62(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	var sb strings.Builder
	sb.Grow(n)
	for _, v := range b {
		sb.WriteByte(base62Chars[int(v)%62])
	}
	return sb.String()
}

// GenerateOpenCodeSessionID generates a descending canonical OpenCode session ID (30 chars).
// The counter is pinned to 1 like the upstream CLI: upstream decodes the
// prefix and rejects a non-small counter with 403 FreeTierError, so an
// incrementing counter would start failing under concurrent load
// (decolua/9router#4842 descendingTimeHex).
func GenerateOpenCodeSessionID() string {
	return "ses_" + descendingTimeHex(uint64(time.Now().UnixMilli()), 1) + unstableRandomBase62(14)
}

// descendingTimeHex renders the 12-hex session prefix: the low 48 bits of
// ~(timeMs * 0x1000 + counter), exactly like the upstream CLI.
func descendingTimeHex(timeMs, counter uint64) string {
	current := timeMs*0x1000 + counter
	value := ^current & (uint64(1)<<48 - 1)
	var timeBytes [6]byte
	for i := range 6 {
		timeBytes[i] = byte((value >> (40 - 8*uint64(i))) & 0xff)
	}
	return hex.EncodeToString(timeBytes[:])
}

// GenerateOpenCodeRequestID generates a canonical OpenCode request ID (30 chars).
func GenerateOpenCodeRequestID() string {
	now := time.Now().UnixMilli()
	current := uint64(now)*0x1000 + 1
	var timeBytes [6]byte
	for i := range 6 {
		timeBytes[i] = byte((current >> (40 - 8*uint64(i))) & 0xff)
	}
	return "msg_" + hex.EncodeToString(timeBytes[:]) + unstableRandomBase62(14)
}

// TranslateOpenCodeSessionID maps foreign sessions to valid 30-char canonical IDs.
// If the input is already canonical, it's preserved (session affinity).
// Otherwise, a fresh timestamped ID is generated — the OpenCode free-tier gate
// rejects non-recent timestamps with 403 FreeTierError, so a deterministic
// hash-based timestamp (which is effectively random) will not pass.
func TranslateOpenCodeSessionID(sessionID, clientTool string) string {
	trimmed := strings.TrimSpace(sessionID)
	if OpenCodeSessionRegex.MatchString(trimmed) {
		return trimmed
	}
	// Not canonical: generate a fresh ID with current timestamp.
	// clientTool is unused for now; kept for API compatibility.
	_ = clientTool
	return GenerateOpenCodeSessionID()
}

var opencodeUARegex = regexp.MustCompile(`(?i)^opencode/([a-z0-9-]+)/(\d+)\.(\d+)\.(\d+)/([a-z0-9-]+)$`)

// HasValidOpenCodeVersion checks whether a User-Agent uses the OpenCode v2 shape.
func HasValidOpenCodeVersion(ua string) bool {
	matches := opencodeUARegex.FindStringSubmatch(ua)
	if len(matches) < 6 {
		return false
	}
	major, err := strconv.Atoi(matches[2])
	if err != nil {
		return false
	}
	return major >= 2
}

// HasValidOpenCodeV1Version checks the pre-v2 User-Agent shape the keyed lanes
// still speak (upstream opencode-zen at decolua/9router#4842): opencode/<major>.<minor>[.patch].
func HasValidOpenCodeV1Version(ua string) bool {
	m := opencodeV1UARegex.FindStringSubmatch(ua)
	if len(m) < 3 {
		return false
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(m[2])
	if err != nil {
		return false
	}
	return major > 1 || (major == 1 && minor >= 17)
}

var opencodeV1UARegex = regexp.MustCompile(`(?i)opencode/(\d+)\.(\d+)(?:\.(\d+))?`)

// openCodeTraceHeaders mints a fresh W3C trace context shaped like the
// OpenCode CLI's: random per request, one 128-bit trace with a 64-bit span,
// b3 carrying the parent span id
// (azrinaldi/opencode-free-bridge server.js buildTraceHeaders).
func openCodeTraceHeaders() (b3, traceparent string) {
	var traceID [16]byte
	var spanID, parentID [8]byte
	_, _ = rand.Read(traceID[:])
	_, _ = rand.Read(spanID[:])
	_, _ = rand.Read(parentID[:])
	tid := hex.EncodeToString(traceID[:])
	sid := hex.EncodeToString(spanID[:])
	pid := hex.EncodeToString(parentID[:])
	return tid + "-" + sid + "-1-" + pid, "00-" + tid + "-" + sid + "-01"
}

// BuildOpenCodeHeaders generates the official OpenCode fingerprint headers to prevent 403 FreeTierError.
// isStream is accepted for signature compatibility; the CLI sends Accept: */*
// on every request (it streams via the body, not the header), so the header
// no longer varies with it.
func BuildOpenCodeHeaders(rawHeaders map[string]string, sessionID string, isStream bool) map[string]string {
	var session string
	if sessionID != "" {
		session = TranslateOpenCodeSessionID(sessionID, "")
	} else {
		session = GenerateOpenCodeSessionID()
	}

	b3, traceparent := openCodeTraceHeaders()
	res := map[string]string{
		"Content-Type":          "application/json",
		"Authorization":         "Bearer public",
		"User-Agent":            DefaultOpenCodeUA,
		"x-opencode-client":     "cli",
		"x-opencode-session":    session,
		"x-opencode-session-id": session,
		"x-session-affinity":    session,
		"x-session-id":          session,
		// Upstream defaults the project to "global"
		// (open-sse/executors/opencode.js buildHeaders).
		"x-opencode-project": "global",
		// The CLI always streams via the body, never via the Accept header,
		// so it sends */* even for streaming requests.
		"Accept":      "*/*",
		"b3":          b3,
		"traceparent": traceparent,
	}
	for _, h := range []string{"x-relay-target", "x-relay-path"} {
		if v, ok := rawHeaders[h]; ok {
			res[h] = v
		}
	}
	for k, v := range rawHeaders {
		lk := strings.ToLower(k)
		if lk == "user-agent" && HasValidOpenCodeVersion(v) {
			res["User-Agent"] = v
		}
		if lk == "x-opencode-session" {
			trimmed := strings.TrimSpace(v)
			if trimmed == "" {
				continue
			}
			hint := trimmed
			if !OpenCodeSessionRegex.MatchString(hint) {
				hint = TranslateOpenCodeSessionID(hint, "")
			}
			// Upstream derives one session and mirrors it on all four
			// headers; a downstream hint must move every mirror together.
			res["x-opencode-session"] = hint
			res["x-opencode-session-id"] = hint
			res["x-session-affinity"] = hint
			res["x-session-id"] = hint
		}
		if lk == "x-opencode-client" {
			res[k] = v
		}
		if lk == "x-opencode-project" {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				res["x-opencode-project"] = trimmed
			}
		}
	}
	return res
}
