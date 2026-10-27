package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultOpenCodeUA is the official OpenCode fingerprint User-Agent.
	// OpenCode v2 fingerprint required by the free-tier /inference/openai/v1 API.
	DefaultOpenCodeUA = "opencode/latest/2.1.4/cli"

	// DefaultOpenCodeV1UA is the pre-v2 identity kept by the keyed lanes
	// (opencode-zen, opencode-go): upstream decolua/9router#4842 moves only
	// the keyless free executor to v2 and leaves every keyed executor on v1.
	DefaultOpenCodeV1UA = "opencode/1.18.31"

	base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// OpenCodeSessionRegex matches canonical OpenCode session IDs: ses_ + 12 hex + 14 Base62 (30 chars).
var OpenCodeSessionRegex = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// OpenCodeSessionDescendingRegex matches the time half of a session id the
// upstream free tier accepts. The gate decodes the 12 hex digits and only
// admits the descending layout the official CLI mints with
// Identifier.descending() — ~(ts_ms<<12|counter), whose low 12 bits read
// 0xFFF-counter — with counter 1–63, i.e. low 12 bits 0xfc0–0xfff. A ses_ id
// with any other encoding (ascending timestamp, sha256 digest, random hex) is
// refused with 403 FreeTierError even though its shape is canonical
// (opencode2api descendingTimePartPattern; dsh-our-free-model measured
// 2026-10-17).
var OpenCodeSessionDescendingRegex = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

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

// IsCanonicalOpenCodeSession reports whether id is a session the upstream free
// tier accepts verbatim: the canonical ses_ shape AND the descending time
// encoding. The gate rejects a shape-valid id whose time half it cannot decode
// as the official client's descending layout, so such an id is re-encoded by
// TranslateOpenCodeSessionID instead of being forwarded (dsh-our-free-model
// sessionForConversation; opencode2api isOfficialSessionID).
func IsCanonicalOpenCodeSession(id string) bool {
	trimmed := strings.TrimSpace(id)
	if !OpenCodeSessionRegex.MatchString(trimmed) {
		return false
	}
	return OpenCodeSessionDescendingRegex.MatchString(trimmed[4:16])
}

// opencodeStableSessionTTL bounds how long an idle stable session is reused
// before a fresh one is minted. A var so tests can shrink the window.
var opencodeStableSessionTTL = 30 * time.Minute

// opencodeStableSessionLimit bounds the stable-session map: at the limit the
// oldest key is evicted. Eviction and expiry are lazy (checked on read), so no
// background goroutine runs for this cache.
const opencodeStableSessionLimit = 1024

var (
	opencodeStableMu       sync.Mutex
	opencodeStableSessions = make(map[string]opencodeStableEntry)
	opencodeStableOrder    []string
)

type opencodeStableEntry struct {
	id       string
	lastUsed time.Time
}

// StableOpenCodeSessionID returns one long-lived canonical session per key,
// minted on first use and reused until it goes stale.
//
// The free tier accounts its quota per upstream session: minting a fresh
// x-opencode-session on every request burns through that budget and surfaces
// as 429 FreeUsageLimitError with a growing retry-after, while the official
// CLI reuses one long-lived session per conversation (decolua/9router
// stableSessionId; dsh-our-free-model sessionForConversation). Executors call
// this with a per-connection key when the client sent no session of its own.
//
// An entry older than opencodeStableSessionTTL is recycled, so a connection
// that idles out starts a fresh budget instead of inheriting an exhausted one.
func StableOpenCodeSessionID(key string) string {
	if key == "" {
		key = "default"
	}
	now := time.Now()
	opencodeStableMu.Lock()
	defer opencodeStableMu.Unlock()
	if entry, ok := opencodeStableSessions[key]; ok && now.Sub(entry.lastUsed) < opencodeStableSessionTTL {
		entry.lastUsed = now
		opencodeStableSessions[key] = entry
		return entry.id
	}
	id := GenerateOpenCodeSessionID()
	opencodeStableSessions[key] = opencodeStableEntry{id: id, lastUsed: now}
	opencodeStableOrder = append(opencodeStableOrder, key)
	for len(opencodeStableOrder) > opencodeStableSessionLimit {
		oldest := opencodeStableOrder[0]
		opencodeStableOrder = opencodeStableOrder[1:]
		delete(opencodeStableSessions, oldest)
	}
	return id
}

// ExpireStableOpenCodeSession retires the stable session minted for key, but
// only while it still holds id: a rotation that loses a race against an
// already-rotated session must not discard the fresh budget that rotation
// minted. It reports whether the entry was removed.
func ExpireStableOpenCodeSession(key, id string) bool {
	opencodeStableMu.Lock()
	defer opencodeStableMu.Unlock()
	entry, ok := opencodeStableSessions[key]
	if !ok || entry.id != id {
		return false
	}
	delete(opencodeStableSessions, key)
	for i, k := range opencodeStableOrder {
		if k == key {
			opencodeStableOrder = append(opencodeStableOrder[:i], opencodeStableOrder[i+1:]...)
			break
		}
	}
	return true
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

// opencodeTranslatedSessionTTL bounds reuse of a translated session id,
// mirroring upstream decolua/9router#5222 (MEMORY_CONFIG.sessionTtlMs, 2 h).
// A var so tests can shrink the window.
var opencodeTranslatedSessionTTL = 2 * time.Hour

// opencodeTranslatedSessionLimit bounds the translated-session memo: at the
// limit the oldest key is evicted. Expiry and eviction are lazy (checked on
// read/insert), so no background goroutine runs for this cache.
const opencodeTranslatedSessionLimit = 5000

var (
	opencodeTranslatedMu       sync.Mutex
	opencodeTranslatedSessions = make(map[string]opencodeTranslatedEntry)
	opencodeTranslatedOrder    []string
)

type opencodeTranslatedEntry struct {
	id     string
	minted time.Time
}

// TranslateOpenCodeSessionID maps a downstream session id onto one the
// upstream free tier accepts. A canonical id — ses_ shape plus the descending
// time encoding the gate decodes — is forwarded verbatim; anything else (a
// UUID, another gateway's session, a ses_ id carrying foreign hex) is minted
// into the descending layout with the CURRENT clock and counter 1, and
// memoized per (clientTool, sessionId) for opencodeTranslatedSessionTTL, so
// one downstream conversation keeps one stable upstream session.
//
// The clock MUST be current: upstream decodes the 12-hex prefix and rejects
// sessions in the future or older than the free-tier window (measured
// upstream at now back to ~-60d; decolua/9router#5222). The previous
// implementation derived the timestamp from a SHA-256 digest of the seed, so
// the decoded clock was uniform over 2^36 ms and most translated ids were
// rejected with 429 FreeUsageLimitError — while clients that sent no session
// (stable per-connection id minted with time.Now()) passed.
func TranslateOpenCodeSessionID(sessionID, clientTool string) string {
	trimmed := strings.TrimSpace(sessionID)
	if IsCanonicalOpenCodeSession(trimmed) {
		return trimmed
	}
	tool := clientTool
	if tool == "" {
		tool = "generic"
	}
	key := tool + "\x00" + trimmed
	now := time.Now()
	opencodeTranslatedMu.Lock()
	defer opencodeTranslatedMu.Unlock()
	if entry, ok := opencodeTranslatedSessions[key]; ok && now.Sub(entry.minted) < opencodeTranslatedSessionTTL {
		return entry.id
	}
	h := sha256.New()
	h.Write([]byte("opencode\x00" + tool + "\x00" + trimmed))
	digest := h.Sum(nil)
	var sb strings.Builder
	sb.Grow(14)
	for i := 6; i < 20; i++ {
		sb.WriteByte(base62Chars[int(digest[i])%62])
	}
	id := "ses_" + descendingTimeHex(uint64(now.UnixMilli()), 1) + sb.String()
	_, exists := opencodeTranslatedSessions[key]
	opencodeTranslatedSessions[key] = opencodeTranslatedEntry{id: id, minted: now}
	if !exists {
		opencodeTranslatedOrder = append(opencodeTranslatedOrder, key)
		for len(opencodeTranslatedOrder) > opencodeTranslatedSessionLimit {
			oldest := opencodeTranslatedOrder[0]
			opencodeTranslatedOrder = opencodeTranslatedOrder[1:]
			delete(opencodeTranslatedSessions, oldest)
		}
	}
	return id
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

func generateTraceHeaders() (string, string) {
	var traceID [16]byte
	var spanID [8]byte
	var parentSpanID [8]byte
	_, _ = rand.Read(traceID[:])
	_, _ = rand.Read(spanID[:])
	_, _ = rand.Read(parentSpanID[:])

	tHex := hex.EncodeToString(traceID[:])
	sHex := hex.EncodeToString(spanID[:])
	pHex := hex.EncodeToString(parentSpanID[:])

	b3 := fmt.Sprintf("%s-%s-1-%s", tHex, sHex, pHex)
	traceparent := fmt.Sprintf("00-%s-%s-01", tHex, sHex)
	return b3, traceparent
}

// BuildOpenCodeHeaders generates the official OpenCode fingerprint headers to prevent 403 FreeTierError.
func BuildOpenCodeHeaders(rawHeaders map[string]string, sessionID string, isStream bool) map[string]string {
	var session string
	if sessionID != "" {
		session = TranslateOpenCodeSessionID(sessionID, "")
	} else {
		session = GenerateOpenCodeSessionID()
	}

	b3, traceparent := generateTraceHeaders()

	res := map[string]string{
		"Content-Type":          "application/json",
		"Authorization":         "Bearer public",
		"User-Agent":            DefaultOpenCodeUA,
		"x-opencode-client":     "cli",
		"x-opencode-session":    session,
		"x-opencode-session-id": session,
		"x-session-affinity":    session,
		"x-session-id":          session,
		"b3":                    b3,
		"traceparent":           traceparent,
		// Upstream defaults the project to "global"
		// (open-sse/executors/opencode.js buildHeaders).
		"x-opencode-project": "global",
	}
	if isStream {
		res["Accept"] = "text/event-stream"
	} else {
		res["Accept"] = "*/*"
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
			if !IsCanonicalOpenCodeSession(hint) {
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
		if lk == "b3" {
			trimmed := strings.TrimSpace(v)
			if trimmed != "" {
				res["b3"] = trimmed
			}
		}
		if lk == "traceparent" {
			trimmed := strings.TrimSpace(v)
			if trimmed != "" {
				res["traceparent"] = trimmed
			}
		}
	}
	return res
}
