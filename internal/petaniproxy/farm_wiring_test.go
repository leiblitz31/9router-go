package petaniproxy

import (
	"context"
	"testing"
)

// Webshare wiring: zero config gets real implementations, not nils.
func TestNewWebshareHunterWithDefaults(t *testing.T) {
	h := NewWebshareHunterWithDefaults(WebshareHunterConfig{})
	if h == nil {
		t.Fatal("expected non-nil hunter")
	}
	if h.solver == nil {
		t.Error("solver is nil, want real GoogleCaptchaSolver")
	}
	if _, ok := h.solver.(*GoogleCaptchaSolver); !ok {
		t.Errorf("solver type = %T, want *GoogleCaptchaSolver", h.solver)
	}
	if h.factory == nil {
		t.Error("factory is nil, want real ChromedpWebshareFactory")
	}
	if _, ok := h.factory.(ChromedpWebshareFactory); !ok {
		t.Errorf("factory type = %T, want ChromedpWebshareFactory", h.factory)
	}
}

// Caller-provided seams are respected, not overwritten.
func TestNewWebshareHunterWithDefaultsKeepsCustom(t *testing.T) {
	custom := &stubCaptchaSolver{}
	h := NewWebshareHunterWithDefaults(WebshareHunterConfig{
		CaptchaSolver: custom,
	})
	if h.solver != custom {
		t.Error("custom solver was overwritten")
	}
}

// Grok wiring: NewBrowser defaults to chromedp driver factory.
func TestGrokFarmWiringBrowserDefault(t *testing.T) {
	cfg := GrokFarmWiring(GrokFarmConfig{})
	if cfg.NewBrowser == nil {
		t.Fatal("NewBrowser is nil, want NewGrokChromedpDriver")
	}
	// Verify it's callable without network (lazy Chromium start).
	drv, err := cfg.NewBrowser(context.Background(), GrokBrowserOptions{Headless: true})
	if err != nil {
		t.Fatalf("NewBrowser() error = %v", err)
	}
	if drv == nil {
		t.Error("driver is nil")
	}
	_ = drv.Close()
}

// Grok wiring: Gmail creds produce a real IMAP email client factory.
func TestGrokFarmWiringEmailDefault(t *testing.T) {
	cfg := GrokFarmWiring(GrokFarmConfig{
		GmailUser:        "user@gmail.com",
		GmailAppPassword: "app-pass",
	})
	if cfg.NewEmailClient == nil {
		t.Fatal("NewEmailClient is nil despite Gmail creds")
	}
	// Construction must not dial (lazy); NewIMAPEmailClient dials on
	// construction, so we only check the factory is wired, not call it.
}

// Grok wiring: no creds → no email client (unchanged behavior).
func TestGrokFarmWiringNoCredsNoEmail(t *testing.T) {
	cfg := GrokFarmWiring(GrokFarmConfig{})
	if cfg.NewEmailClient != nil {
		t.Error("NewEmailClient set without creds, want nil")
	}
}

// Zai wiring: real chromedp driver, no Chromium needed at construction.
func TestNewZaiFarmWithDefaults(t *testing.T) {
	farm, err := NewZaiFarmWithDefaults(context.Background(), ZaiFarmDefaults{
		Headless: true,
	})
	if err != nil {
		t.Fatalf("NewZaiFarmWithDefaults() error = %v", err)
	}
	if farm == nil {
		t.Fatal("expected non-nil farm")
	}
	if farm.Driver == nil {
		t.Error("Driver is nil, want real chromedp driver")
	}
}

// stubCaptchaSolver is a minimal WebshareCaptchaSolver for override tests.
type stubCaptchaSolver struct{}

func (s *stubCaptchaSolver) SolveAudio(ctx context.Context, audio []byte) (WebshareAudioTranscript, error) {
	return WebshareAudioTranscript{Text: "stub"}, nil
}
