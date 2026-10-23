package providers

import "testing"

// TestCommandCodeLiveCatalog pins the live-catalogue wiring for Command Code.
//
// Its registry catalog is a fixed 22-model snapshot, so every model Command
// Code released after that snapshot exists only in the Provider API's
// /v1/models feed: the free tiers (reflection/beam:free,
// stealth/glyph-cluster:free, poolside/laguna-s-2.1-free) and the paid ones
// (minimax/minimax-m3.1-flash, z-ai/glm-5.3-flash, moonshotai/Kimi-K3, …).
// Without the endpoint below, the dashboard's "Check Latest Models", "Import
// from /models" and "Sync Models" all answer 400 for the provider and the
// catalogue stays frozen at the snapshot.
func TestCommandCodeLiveCatalog(t *testing.T) {
	const want = "https://api.commandcode.ai/provider/v1/models"
	if got := ModelsListURL("commandcode"); got != want {
		t.Errorf("ModelsListURL(commandcode) = %q, want %q", got, want)
	}
	// The endpoint answers keyless, so the catalogue is reachable even for a
	// connection whose credential was revoked — a discovery path that needed a
	// live key would report a broken sync instead of a stale catalogue.
	if got := ModelsListHeaders("commandcode"); got != nil {
		t.Errorf("ModelsListHeaders(commandcode) = %v, want nil (no extra headers)", got)
	}
}
