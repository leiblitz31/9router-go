package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

// Upstream decolua/9router#5193: the dashboard Test for the free OpenCode
// lane used to probe https://opencode.ai/zen/v1/models with the v1
// "opencode/1.18.31" fingerprint upstream rejects with 403 FreeTierError,
// and the reporter saw the IP locked out right after using Test. The fix
// upstream probes with one stealth chat completion instead (x-opencode-client:
// cli, opencode/latest UA). This test pins that the probe stays the stealth
// one — exactly one call, to the chat completions URL, never a GET to
// /zen/v1/models — so a future probe-matrix change cannot silently
// reintroduce the lockout-prone probe.
func TestHandleValidateProvider_OpencodeFreeLaneStealthProbe(t *testing.T) {
	calls := withProbeStub(t, func(call int, p capturedProbe) (int, []byte, error) {
		return http.StatusOK, []byte(`data: {"choices":[{"delta":{"content":"Hello!"}}]}\n\n`), nil
	})

	rec, out := postValidate(t, `{"provider":"opencode","apiKey":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if out["valid"] != true {
		t.Errorf("expected valid=true for the free opencode lane, got %v", out["valid"])
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 probe for the free opencode lane, got %d", len(*calls))
	}
	p := (*calls)[0]
	if p.url != "https://opencode.ai/inference/openai/v1/chat/completions" {
		t.Errorf("unexpected probe url %s", p.url)
	}
	if strings.Contains(p.url, "/zen/v1/models") {
		t.Errorf("probe must not hit /zen/v1/models: %s", p.url)
	}
	if p.headers.Get("x-opencode-client") != "cli" {
		t.Errorf("probe missing stealth x-opencode-client header: %v", p.headers)
	}
	if !strings.HasPrefix(p.headers.Get("User-Agent"), "opencode/") {
		t.Errorf("probe missing stealth opencode User-Agent: %v", p.headers)
	}
}
