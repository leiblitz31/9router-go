package dashboard

import (
	"net/http"
	"testing"
)

// Upstream decolua/9router#5193: the dashboard Test for the free OpenCode
// lane used to probe https://opencode.ai/zen/v1/models with the v1
// "opencode/1.18.31" fingerprint upstream rejects with 403 FreeTierError,
// and the reporter saw the IP locked out right after using Test. The fix
// upstream returns valid with no network call. This fork's NoAuth branch
// already returns valid without probing; this test pins that behavior for
// the opencode free lane specifically, so a future probe-matrix change
// cannot silently reintroduce the probe.
func TestHandleValidateProvider_OpencodeFreeLaneMakesNoProbe(t *testing.T) {
	calls := staticProbeStub(t, http.StatusInternalServerError)

	rec, out := postValidate(t, `{"provider":"opencode","apiKey":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if out["valid"] != true {
		t.Errorf("expected valid=true for the free opencode lane, got %v", out["valid"])
	}
	if len(*calls) != 0 {
		t.Errorf("expected no probe for the free opencode lane, got %d", len(*calls))
	}
}
