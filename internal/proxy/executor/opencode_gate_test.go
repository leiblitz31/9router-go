package executor

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"9router/proxy/internal/proxy"
)

func gateTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Body:          io.NopCloser(strings.NewReader(body)),
		Header:        make(http.Header),
		ContentLength: int64(len(body)),
	}
}

func resetOpencodeGateCooldown() {
	opencodeGateMu.Lock()
	opencodeGateRetryAt = time.Time{}
	opencodeGateMu.Unlock()
}

func TestIsOpencodeFreeTierError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"gate rejection", 403, `{"error":{"type":"FreeTierError","message":"x"}}`, true},
		{"gate message text", 403, `OpenCode's free tier can only be used from within OpenCode`, true},
		{"other 403", 403, `{"error":"quota exceeded"}`, false},
		{"429 is not a gate rejection", 429, `{"error":{"type":"FreeTierError"}}`, false},
		{"200 never", 200, ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOpencodeFreeTierError(tt.status, []byte(tt.body)); got != tt.want {
				t.Errorf("isOpencodeFreeTierError(%d, %q) = %v, want %v", tt.status, tt.body, got, tt.want)
			}
		})
	}
}

func TestOpencodeGateRequest_RetriesOnceOnGate403(t *testing.T) {
	resetOpencodeGateCooldown()
	var sessions, bodies []string
	send := func(session string, body []byte) (*http.Response, error) {
		sessions = append(sessions, session)
		bodies = append(bodies, string(body))
		if len(sessions) == 1 {
			return gateTestResponse(403, `{"error":{"type":"FreeTierError","message":"free tier"}}`), nil
		}
		return gateTestResponse(200, `ok`), nil
	}
	firstMap := map[string]string{"read_client": "read"}
	escalated := []byte(`{"escalated":true}`)
	escalate := func() ([]byte, map[string]string) {
		return escalated, map[string]string{}
	}

	resp, outMap, outSession, err := opencodeGateRequest(send, "ses_first", []byte(`{"a":1}`), firstMap, escalate)
	if err != nil {
		t.Fatalf("opencodeGateRequest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected retried 200, got %d", resp.StatusCode)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(sessions))
	}
	if sessions[1] == sessions[0] {
		t.Errorf("retry must mint a fresh session, got same %q", sessions[1])
	}
	if !proxy.OpenCodeSessionRegex.MatchString(sessions[1]) {
		t.Errorf("retry session %q is not canonical", sessions[1])
	}
	if bodies[1] != string(escalated) {
		t.Errorf("retry must send the escalated body, got %q", bodies[1])
	}
	if len(outMap) != 0 {
		t.Errorf("expected escalated tool map, got %v", outMap)
	}
	if outSession != sessions[1] {
		t.Errorf("returned session %q != retry session %q", outSession, sessions[1])
	}
}

func TestOpencodeGateRequest_NoRetryOnPlain403(t *testing.T) {
	resetOpencodeGateCooldown()
	calls := 0
	send := func(session string, body []byte) (*http.Response, error) {
		calls++
		return gateTestResponse(403, `{"error":"account suspended"}`), nil
	}
	resp, _, _, err := opencodeGateRequest(send, "s", []byte(`{}`), nil,
		func() ([]byte, map[string]string) { return nil, nil })
	if err != nil {
		t.Fatalf("opencodeGateRequest: %v", err)
	}
	defer resp.Body.Close()
	if calls != 1 {
		t.Errorf("plain 403 must not retry, got %d calls", calls)
	}
	// Body must be intact for the caller's status handling.
	restored, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(restored), "account suspended") {
		t.Errorf("403 body not preserved, got %q", restored)
	}
}

func TestOpencodeGateRequest_CooldownBlocksRepeatRetry(t *testing.T) {
	resetOpencodeGateCooldown()
	send := func(session string, body []byte) (*http.Response, error) {
		return gateTestResponse(403, `{"error":{"type":"FreeTierError"}}`), nil
	}
	escalate := func() ([]byte, map[string]string) { return []byte(`{}`), nil }

	// First request retries once (2 calls) and arms the cooldown.
	calls := 0
	counting := func(session string, body []byte) (*http.Response, error) {
		calls++
		return send(session, body)
	}
	resp, _, _, err := opencodeGateRequest(counting, "s", []byte(`{}`), nil, escalate)
	if err != nil {
		t.Fatalf("opencodeGateRequest: %v", err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Fatalf("expected first request to retry once (2 calls), got %d", calls)
	}

	// A second gate 403 inside the window must not retry again.
	calls = 0
	resp2, _, _, err := opencodeGateRequest(counting, "s", []byte(`{}`), nil, escalate)
	if err != nil {
		t.Fatalf("opencodeGateRequest: %v", err)
	}
	defer resp2.Body.Close()
	if calls != 1 {
		t.Errorf("cooldown must suppress the second retry, got %d calls", calls)
	}
	if resp2.StatusCode != 403 {
		t.Errorf("expected the original 403 back, got %d", resp2.StatusCode)
	}
}

func TestOpencodeGateRequest_SuccessPassthrough(t *testing.T) {
	calls := 0
	send := func(session string, body []byte) (*http.Response, error) {
		calls++
		return gateTestResponse(200, `ok`), nil
	}
	m := map[string]string{"a": "b"}
	resp, outMap, outSession, err := opencodeGateRequest(send, "ses_x", []byte(`{}`), m,
		func() ([]byte, map[string]string) { return nil, nil })
	if err != nil {
		t.Fatalf("opencodeGateRequest: %v", err)
	}
	defer resp.Body.Close()
	if calls != 1 || resp.StatusCode != 200 {
		t.Errorf("success must pass through untouched: calls=%d status=%d", calls, resp.StatusCode)
	}
	if outSession != "ses_x" || len(outMap) != 1 {
		t.Errorf("success must return the original map/session, got %v %q", outMap, outSession)
	}
}

func TestOpencodeGateRequest_TransportError(t *testing.T) {
	send := func(session string, body []byte) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	}
	_, _, _, err := opencodeGateRequest(send, "s", []byte(`{}`), nil,
		func() ([]byte, map[string]string) { return nil, nil })
	if err == nil {
		t.Errorf("transport error must propagate")
	}
}
