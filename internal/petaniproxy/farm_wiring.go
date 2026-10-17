// farm_wiring.go — production wiring helpers for the farm hunter structs.
//
// The ported farm modules (webshare, grok, zai) were written against small
// interfaces (browser drivers, captcha solver, email client) so they stay
// testable. This file provides the constructors that plug in the REAL
// implementations:
//
//   - Browser: chromedp (ChromedpWebshareFactory, NewGrokChromedpDriver,
//     NewZaiChromedpDriver)
//   - Captcha: Google Web Speech API, gratis (NewGoogleCaptchaSolver)
//   - Email: go-imap (NewIMAPEmailClient)
//
// Nothing here touches the network at construction time; browsers and IMAP
// connections are lazy.
package petaniproxy

import (
	"context"
	"fmt"
)

// ---------------------------------------------------------------------------
// Webshare
// ---------------------------------------------------------------------------

// NewWebshareHunterWithDefaults builds a WebshareHunter with production
// implementations wired in. Any zero-value seam in cfg is filled:
//
//   - BrowserFactory → ChromedpWebshareFactory{}
//   - CaptchaSolver  → NewGoogleCaptchaSolver() (gratis Google Web Speech API)
//
// Other cfg fields keep NewWebshareHunter's own defaults.
func NewWebshareHunterWithDefaults(cfg WebshareHunterConfig) *WebshareHunter {
	if cfg.BrowserFactory == nil {
		cfg.BrowserFactory = ChromedpWebshareFactory{}
	}
	if cfg.CaptchaSolver == nil {
		cfg.CaptchaSolver = NewGoogleCaptchaSolver()
	}
	return NewWebshareHunter(cfg)
}

// ---------------------------------------------------------------------------
// Zai
// ---------------------------------------------------------------------------

// ZaiFarmDefaults carries the knobs for NewZaiFarmWithDefaults.
type ZaiFarmDefaults struct {
	// Headless launches Chromium without a window.
	Headless bool
	// Proxy is the --proxy-server value, "" for none.
	Proxy string
	// UserAgent overrides the browser UA, "" for chromedp default.
	UserAgent string
	// Store persists accounts; nil disables DB persistence.
	Store ZaiAccountStore
	// Sink receives the email:password:token lines; nil disables.
	Sink ZaiAccountFileSink
}

// NewZaiFarmWithDefaults builds a ZaiFarm with a real chromedp driver.
// The driver is created immediately (it lazy-starts Chromium on first use).
func NewZaiFarmWithDefaults(ctx context.Context, d ZaiFarmDefaults) (*ZaiFarm, error) {
	driver, err := NewZaiChromedpDriver(ctx, ZaiChromedpOptions{
		Headless:  d.Headless,
		Proxy:     d.Proxy,
		UserAgent: d.UserAgent,
	})
	if err != nil {
		return nil, fmt.Errorf("farm_wiring: NewZaiChromedpDriver: %w", err)
	}
	return NewZaiFarm(driver, d.Store, d.Sink), nil
}

// ---------------------------------------------------------------------------
// Grok
// ---------------------------------------------------------------------------

// GrokFarmWiring fills the browser/email seams of a GrokFarmConfig with
// production implementations. It returns the config with defaults applied:
//
//   - NewBrowser → NewGrokChromedpDriver (when nil)
//   - NewEmailClient → go-imap Gmail client (when nil AND GmailUser +
//     GmailAppPassword are set)
//
// Callers then pass the result to whatever runs the farm (e.g. RunGrokFarm).
func GrokFarmWiring(cfg GrokFarmConfig) GrokFarmConfig {
	if cfg.NewBrowser == nil {
		cfg.NewBrowser = NewGrokChromedpDriver
	}
	if cfg.NewEmailClient == nil && cfg.GmailUser != "" && cfg.GmailAppPassword != "" {
		user := cfg.GmailUser
		pass := cfg.GmailAppPassword
		cfg.NewEmailClient = func(ctx context.Context) (EmailClient, error) {
			// ctx is accepted for signature parity; dial has its own timeout.
			_ = ctx
			return NewIMAPEmailClient("imap.gmail.com:993", user, pass, true)
		}
	}
	return cfg
}
