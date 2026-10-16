package chat

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"9router/proxy/internal/log"
	"9router/proxy/internal/providers"
	internalproxy "9router/proxy/internal/proxy"
)

// Cursor empty-turn recovery (decolua/9router#4690).
//
// A long session can get HTTP 200 with zero content: the request is accepted
// but the turn carries no answer. The empty-response kernel reports that as a
// retryable 502 instead of serving it; this layer spends a retry on a
// tail-only frame (system prompts + latest turns) before failing the turn.
// Short sessions skip the retry: a first-message empty turn is a protocol
// condition, not a wedged replay.
//
// Streaming turns are excluded: SSE headers commit before the first chunk,
// so a second attempt would concatenate two streams under one header.
//
// Ponytail: no retry delay by default beyond CURSOR_EMPTY_TURN_RETRY_DELAY_MS
// (400ms); drop it to 0 for tests via env.

// cursorEmptyTurnMaxRetries caps recovery attempts (default 1, 0 disables).
func cursorEmptyTurnMaxRetries() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CURSOR_EMPTY_TURN_MAX_RETRIES"))); err == nil && v >= 0 {
		return v
	}
	return 1
}

// cursorTailOnlyThreshold is the replayed-message count at/above which a
// retry is attempted (default 80).
func cursorTailOnlyThreshold() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CURSOR_TAIL_ONLY_THRESHOLD"))); err == nil && v > 0 {
		return v
	}
	return 80
}

func cursorRetryDelay() time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CURSOR_EMPTY_TURN_RETRY_DELAY_MS"))); err == nil && v >= 0 {
		return time.Duration(v) * time.Millisecond
	}
	return 400 * time.Millisecond
}

// isCursorEmptyTurnError reports whether err is the kernel's empty-turn
// failure: a 200 that carried no completion, surfaced as a 502.
func isCursorEmptyTurnError(err error) bool {
	var ue *internalproxy.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	if ue.StatusCode != http.StatusBadGateway {
		return false
	}
	body := string(ue.Body)
	return strings.Contains(body, "without a completion") ||
		strings.Contains(body, "no completion chunk") ||
		strings.Contains(body, "empty body")
}

// tailOnlyCursorBody rebuilds a long-session body as system prompts plus the
// latest turns, with a trim notice. It returns nil when the session is below
// the retry threshold or the body does not parse — the caller then fails
// open with the original error.
func tailOnlyCursorBody(body []byte) []byte {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	raw, ok := req["messages"].([]any)
	if !ok || len(raw) < cursorTailOnlyThreshold() {
		return nil
	}
	const keepTail = 8
	var systems []any
	for _, m := range raw {
		obj, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := obj["role"].(string)
		if role != "system" {
			continue
		}
		if content, _ := obj["content"].(string); strings.Contains(content, "[9router trim]") {
			continue // idempotent re-trim: never stack notices
		}
		systems = append(systems, m)
	}
	start := len(raw) - keepTail
	if start < 0 {
		start = 0
	}
	var recent []any
	for _, m := range raw[start:] {
		if obj, ok := m.(map[string]any); ok {
			if role, _ := obj["role"].(string); role == "system" {
				continue
			}
		}
		recent = append(recent, m)
	}
	notice := map[string]any{
		"role":    "system",
		"content": "[9router trim] earlier history trimmed to recover this turn; the full replay returned an empty turn",
	}
	req["messages"] = append(append(systems, notice), recent...)
	out, err := json.Marshal(req)
	if err != nil {
		return nil
	}
	return out
}

// retryCursorEmptyTurn runs the empty-turn recovery: one tail-only retry per
// attempt while the failure is still the kernel's empty-turn 502. Any other
// outcome — success, a different error, no tail frame — is returned as-is.
func (h *ChatHandler) retryCursorEmptyTurn(
	ctx context.Context, w http.ResponseWriter,
	cfg *providers.ProviderConfig, apiKey string, body []byte,
	isStream bool, translateResponse bool, metrics *streamMetrics,
	httpClient *http.Client, firstErr error,
) error {
	if isStream || ctx.Err() != nil || !isCursorEmptyTurnError(firstErr) {
		return firstErr
	}
	tail := tailOnlyCursorBody(body)
	if tail == nil {
		return firstErr
	}
	delay := cursorRetryDelay()
	err := firstErr
	for attempt := 0; attempt < cursorEmptyTurnMaxRetries(); attempt++ {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(delay):
			}
		}
		log.Warn("fallback", "cursor empty turn, retrying tail-only", "attempt", attempt+1)
		err = h.forwardRequest(ctx, w, cfg, apiKey, tail, isStream, translateResponse, metrics, httpClient)
		if err == nil || !isCursorEmptyTurnError(err) {
			return err
		}
	}
	return err
}
