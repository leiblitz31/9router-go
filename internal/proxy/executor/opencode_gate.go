package executor

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"time"

	"9router/proxy/internal/proxy"
)

// opencodeGateRetryCooldown bounds the reactive 403 self-heal: retries inside
// the window are skipped so a per-IP upstream penalty is never amplified by
// duplicate retries (azrinaldi/opencode-free-bridge GATE_RETRY_COOLDOWN_MS).
const opencodeGateRetryCooldown = 30 * time.Second

var (
	opencodeGateMu      sync.Mutex
	opencodeGateRetryAt time.Time
)

// opencodeGateRetryAllowed reports whether a 403 self-heal retry may fire now,
// recording the attempt. At most one retry per cooldown window, process-wide.
func opencodeGateRetryAllowed() bool {
	opencodeGateMu.Lock()
	defer opencodeGateMu.Unlock()
	if time.Since(opencodeGateRetryAt) < opencodeGateRetryCooldown {
		return false
	}
	opencodeGateRetryAt = time.Now()
	return true
}

// isOpencodeFreeTierError reports whether a response is the upstream free-tier
// gate rejection ("OpenCode's free tier can only be used from within
// OpenCode", error type FreeTierError) as opposed to any other 403.
func isOpencodeFreeTierError(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("freetier")) ||
		bytes.Contains(lower, []byte("free tier can only be used"))
}

// opencodeGateRequest sends an opencode free-tier request via send and, on a
// 403 FreeTierError, retries once with a fresh session ID and the full CLI
// tool profile (escalate rebuilds the body from the pre-concealment request).
// It returns the final response with an open body (the caller closes it),
// the tool-name map and session ID that produced it.
//
// send performs one POST for the given session ID and body; escalate returns
// the full-tool-set body plus its tool-name map. Retries inside the cooldown
// window are skipped and the original 403 is returned with its body intact.
func opencodeGateRequest(
	send func(session string, body []byte) (*http.Response, error),
	session string,
	body []byte,
	toolNameMap map[string]string,
	escalate func() ([]byte, map[string]string),
) (*http.Response, map[string]string, string, error) {
	resp, err := send(session, body)
	if err != nil {
		return nil, nil, "", err
	}
	if resp.StatusCode != http.StatusForbidden {
		return resp, toolNameMap, session, nil
	}
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	resp.Body.Close()
	if !isOpencodeFreeTierError(http.StatusForbidden, errBody) || !opencodeGateRetryAllowed() {
		// Not a gate rejection, or cooling down: hand the 403 back with its
		// body restored so the caller's status handling sees it untouched.
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
		resp.ContentLength = int64(len(errBody))
		return resp, toolNameMap, session, nil
	}
	// Self-heal: evict the rejected session, mint a fresh one, and retry once
	// with the full tool profile, like the bridge's reactive self-heal.
	newSession := proxy.GenerateOpenCodeSessionID()
	newBody, newMap := escalate()
	resp2, err := send(newSession, newBody)
	if err != nil {
		return nil, nil, "", err
	}
	return resp2, newMap, newSession, nil
}
