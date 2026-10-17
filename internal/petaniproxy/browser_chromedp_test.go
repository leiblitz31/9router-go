package petaniproxy

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
)

// offlineBrowser returns a chromedpBrowser whose context is NOT a chromedp
// context: every CDP-touching method must fail fast without launching a
// browser or touching the network.
func offlineBrowser() *chromedpBrowser {
	return &chromedpBrowser{
		ctx:          context.Background(),
		rnd:          rand.New(rand.NewSource(1)),
		typePauseMin: 0,
		typePauseMax: 0,
	}
}

func TestChromedpTranslateSelector(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		want     string
		wantX    bool
	}{
		{name: "attr name", selector: "@name=email", want: `[name="email"]`},
		{name: "attr type", selector: "@type=email", want: `[type="email"]`},
		{name: "tag plus attr", selector: "tag:input@type=checkbox", want: `input[type="checkbox"]`},
		{name: "class passthrough", selector: ".PrivateSwitchBase-input", want: ".PrivateSwitchBase-input"},
		{name: "bare text", selector: "text:Sign Up With Email", want: `//*[contains(.,'Sign Up With Email')]`, wantX: true},
		{name: "bare text short", selector: "text:Download", want: `//*[contains(.,'Download')]`, wantX: true},
		{name: "tag plus text call", selector: "tag:button@@text():Download", want: `//button[contains(.,'Download')]`, wantX: true},
		{name: "tag p plus text call", selector: "tag:p@@text():Proxy List", want: `//p[contains(.,'Proxy List')]`, wantX: true},
		{name: "empty", selector: "", want: ""},
		{name: "whitespace trimmed", selector: "  @name=email  ", want: `[name="email"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, isX := translateWebshareSelector(tt.selector)
			if got != tt.want {
				t.Errorf("translateWebshareSelector(%q) = %q, want %q", tt.selector, got, tt.want)
			}
			if isX != tt.wantX {
				t.Errorf("translateWebshareSelector(%q) isXPath = %v, want %v", tt.selector, isX, tt.wantX)
			}
		})
	}
}

func TestChromedpTranslateSelectorQuoteEscape(t *testing.T) {
	got, isX := translateWebshareSelector(`text:it's quoted`)
	if !isX {
		t.Fatalf("expected XPath for text: selector")
	}
	if !strings.Contains(got, "concat(") {
		t.Errorf("expected concat() quoting for embedded single quote, got %q", got)
	}
}

func TestChromedpChromiumFlags(t *testing.T) {
	t.Run("headless adds headless flags and window size", func(t *testing.T) {
		cfg := chromedpLaunchConfig{Headless: true, Args: []string{"--window-size=1920,1080"}}
		flags := cfg.chromiumFlags()
		for _, want := range []string{"--headless", "--hide-scrollbars", "--mute-audio", "--window-size=1920,1080"} {
			if !hasChromiumFlag(flags, want) {
				t.Errorf("flags %v missing %q", flags, want)
			}
		}
	})
	t.Run("headed has no headless flag", func(t *testing.T) {
		cfg := chromedpLaunchConfig{Args: []string{"--start-maximized"}}
		if hasChromiumFlag(cfg.chromiumFlags(), "--headless") {
			t.Errorf("headed flags must not contain --headless: %v", cfg.chromiumFlags())
		}
	})
	t.Run("proxy added once", func(t *testing.T) {
		cfg := chromedpLaunchConfig{Proxy: "http://127.0.0.1:8080"}
		flags := cfg.chromiumFlags()
		count := 0
		for _, f := range flags {
			if strings.HasPrefix(f, "--proxy-server=") {
				count++
				if f != "--proxy-server=http://127.0.0.1:8080" {
					t.Errorf("unexpected proxy flag %q", f)
				}
			}
		}
		if count != 1 {
			t.Errorf("expected exactly one --proxy-server flag, got %d in %v", count, flags)
		}
	})
	t.Run("proxy not duplicated when already in args", func(t *testing.T) {
		cfg := chromedpLaunchConfig{
			Args:  []string{"--proxy-server=http://127.0.0.1:8080"},
			Proxy: "http://127.0.0.1:8080",
		}
		count := 0
		for _, f := range cfg.chromiumFlags() {
			if strings.HasPrefix(f, "--proxy-server=") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected exactly one --proxy-server flag, got %d", count)
		}
	})
	t.Run("user agent and extensions", func(t *testing.T) {
		cfg := chromedpLaunchConfig{
			UserAgent:  "TestAgent/1.0",
			Extensions: []string{"/tmp/ext1", "", "/tmp/ext2"},
		}
		flags := cfg.chromiumFlags()
		if !hasChromiumFlag(flags, "--user-agent") {
			t.Errorf("flags %v missing --user-agent", flags)
		}
		extCount := 0
		for _, f := range flags {
			if strings.HasPrefix(f, "--load-extension=") {
				extCount++
			}
		}
		if extCount != 2 {
			t.Errorf("expected 2 --load-extension flags (empty skipped), got %d in %v", extCount, flags)
		}
	})
	t.Run("exec path", func(t *testing.T) {
		cfg := chromedpLaunchConfig{ExecPath: "/usr/bin/chromium"}
		if !hasChromiumFlag(cfg.chromiumFlags(), "--exec-path") {
			t.Errorf("flags %v missing --exec-path", cfg.chromiumFlags())
		}
	})
}

func TestChromedpAllocatorOpts(t *testing.T) {
	cfg := chromedpLaunchConfig{
		Headless:   true,
		Args:       []string{"--no-sandbox", "--window-size=1920,1080"},
		UserAgent:  "UA",
		Proxy:      "http://127.0.0.1:8080",
		Extensions: []string{"/tmp/ext"},
		ExecPath:   "/usr/bin/chromium",
	}
	opts := cfg.allocatorOpts()
	if len(opts) == 0 {
		t.Fatalf("allocatorOpts() returned no options")
	}
	// Base options + exec path + headless + UA + proxy + 2 args + 1 extension.
	if want := 2 + 1 + 1 + 1 + 1 + 2 + 1; len(opts) != want {
		t.Errorf("allocatorOpts() len = %d, want %d", len(opts), want)
	}
}

func TestChromedpSplitChromiumFlag(t *testing.T) {
	name, val := splitChromiumFlag("--proxy-server=http://x:8080")
	if name != "proxy-server" || val != "http://x:8080" {
		t.Errorf("got (%q,%v)", name, val)
	}
	name, val = splitChromiumFlag("--no-sandbox")
	if name != "no-sandbox" || val != true {
		t.Errorf("got (%q,%v)", name, val)
	}
}

func TestChromedpHumanTypePause(t *testing.T) {
	rnd := rand.New(rand.NewSource(42))
	for i := 0; i < 200; i++ {
		d := humanTypePause(rnd, 30*time.Millisecond, 80*time.Millisecond)
		if d < 30*time.Millisecond || d >= 80*time.Millisecond {
			t.Fatalf("pause %v out of [30ms,80ms)", d)
		}
	}
	if got := humanTypePause(rnd, 50*time.Millisecond, 10*time.Millisecond); got != 50*time.Millisecond {
		t.Errorf("max<=min should return min, got %v", got)
	}
}

func TestChromedpWrapRunJSScript(t *testing.T) {
	t.Run("args encoded and arguments[0] usable", func(t *testing.T) {
		wrapped, err := wrapRunJSScript(`const email = arguments[0]; return email;`, []any{"a@b.c"})
		if err != nil {
			t.Fatalf("wrapRunJSScript: %v", err)
		}
		if !strings.Contains(wrapped, `["a@b.c"]`) {
			t.Errorf("args JSON missing from wrapper: %q", wrapped)
		}
		if !strings.Contains(wrapped, "arguments[0]") {
			t.Errorf("script body missing from wrapper: %q", wrapped)
		}
		if !strings.HasPrefix(wrapped, "(function(){") || !strings.HasSuffix(wrapped, "})()") {
			t.Errorf("wrapper shape wrong: %q", wrapped)
		}
	})
	t.Run("no args", func(t *testing.T) {
		wrapped, err := wrapRunJSScript(`return 1;`, nil)
		if err != nil {
			t.Fatalf("wrapRunJSScript: %v", err)
		}
		if !strings.Contains(wrapped, "[]") {
			t.Errorf("expected empty args array in %q", wrapped)
		}
	})
	t.Run("unmarshalable arg errors", func(t *testing.T) {
		_, err := wrapRunJSScript(`return 1;`, []any{func() {}})
		if err == nil {
			t.Errorf("expected error for func arg, got nil")
		}
	})
}

func TestChromedpQuadToRect(t *testing.T) {
	q := dom.Quad{10, 20, 110, 20, 110, 70, 10, 70}
	r := quadToWebshareRect(q)
	if r.X != 10 || r.Y != 20 || r.W != 100 || r.H != 50 {
		t.Errorf("quadToWebshareRect = %+v, want {10 20 100 50}", r)
	}
	if r := quadToWebshareRect(dom.Quad{1, 2}); r != (WebshareRect{}) {
		t.Errorf("short quad should give zero rect, got %+v", r)
	}
	cx, cy := rectCenter(WebshareRect{X: 10, Y: 20, W: 100, H: 50})
	if cx != 60 || cy != 45 {
		t.Errorf("rectCenter = (%v,%v), want (60,45)", cx, cy)
	}
}

func TestChromedpGrokCookieFromCDP(t *testing.T) {
	c := &network.Cookie{
		Name: "session", Value: "abc", Domain: ".x.ai", Path: "/",
		Secure: true, HTTPOnly: true, Expires: 1893456000,
	}
	g := grokCookieFromCDP(c)
	if g.Name != "session" || g.Value != "abc" || g.Domain != ".x.ai" || g.Path != "/" {
		t.Errorf("identity fields wrong: %+v", g)
	}
	if !g.Secure || !g.HTTPOnly {
		t.Errorf("flags wrong: %+v", g)
	}
	if g.Expires != 1893456000 {
		t.Errorf("expires = %d, want 1893456000", g.Expires)
	}
	if g := grokCookieFromCDP(&network.Cookie{Name: "s", Expires: -1}); g.Expires != 0 {
		t.Errorf("session cookie expires = %d, want 0 (omitted)", g.Expires)
	}
}

func TestChromedpCollectFrameIDs(t *testing.T) {
	tree := &page.FrameTree{
		Frame: &cdp.Frame{ID: "main"},
		ChildFrames: []*page.FrameTree{
			{Frame: &cdp.Frame{ID: "f1"}},
			{Frame: &cdp.Frame{ID: "f2"}, ChildFrames: []*page.FrameTree{
				{Frame: &cdp.Frame{ID: "f2a"}},
			}},
		},
	}
	var ids []string
	collectFrameIDs(tree, &ids)
	want := []string{"main", "f1", "f2", "f2a"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
	var nilIDs []string
	collectFrameIDs(nil, &nilIDs)
	if len(nilIDs) != 0 {
		t.Errorf("nil tree should append nothing, got %v", nilIDs)
	}
}

// TestChromedpOfflineErrorPaths ensures every driver method fails fast with a
// plain (non-chromedp) context: no browser launch, no network, no hang.
func TestChromedpOfflineErrorPaths(t *testing.T) {
	ctx := context.Background()
	b := offlineBrowser()
	grok := &chromedpGrokDriver{b}
	zai := &chromedpZaiDriver{b}

	t.Run("webshare browser methods error", func(t *testing.T) {
		if err := b.Navigate(ctx, "https://example.com"); err == nil {
			t.Errorf("Navigate: expected error")
		}
		if _, err := b.URL(ctx); err == nil {
			t.Errorf("URL: expected error")
		}
		if _, err := b.Eval(ctx, "1+1"); err == nil {
			t.Errorf("Eval: expected error")
		}
		if _, err := b.EvalInFrame(ctx, "frame1", "1+1"); err == nil {
			t.Errorf("EvalInFrame: expected error")
		}
		if _, err := b.Frames(ctx); err == nil {
			t.Errorf("Frames: expected error")
		}
		if err := b.DispatchMouseMove(ctx, 10, 20); err == nil {
			t.Errorf("DispatchMouseMove: expected error")
		}
		if _, err := b.FindElement(ctx, "@name=email"); err == nil {
			t.Errorf("FindElement: expected error")
		} else if !errors.Is(err, ErrWebshareElementNotFound) && !strings.Contains(err.Error(), "find element") {
			t.Errorf("FindElement: unexpected error %v", err)
		}
	})
	t.Run("webshare element methods error", func(t *testing.T) {
		el := &chromedpElement{browser: b, nodeID: 1}
		if err := el.Click(ctx); err == nil {
			t.Errorf("Click: expected error")
		}
		if _, ok := el.Rect(ctx); ok {
			t.Errorf("Rect: expected ok=false")
		}
		if err := el.Clear(ctx); err == nil {
			t.Errorf("Clear: expected error")
		}
		if err := el.TypeChar(ctx, "a"); err == nil {
			t.Errorf("TypeChar: expected error")
		}
		if err := el.TypeChar(ctx, ""); err != nil {
			t.Errorf("TypeChar empty: expected nil, got %v", err)
		}
	})
	t.Run("grok driver methods error", func(t *testing.T) {
		if _, err := grok.RunJS(ctx, `return arguments[0];`, "x"); err == nil {
			t.Errorf("RunJS: expected error")
		}
		if _, err := grok.Cookies(ctx); err == nil {
			t.Errorf("Cookies: expected error")
		}
	})
	t.Run("zai driver methods error", func(t *testing.T) {
		if found, err := zai.Fill(ctx, `//input`, "v"); err == nil || found {
			t.Errorf("Fill: expected (false, err), got (%v,%v)", found, err)
		}
		if found, err := zai.Click(ctx, `//button`); err == nil || found {
			t.Errorf("Click: expected (false, err), got (%v,%v)", found, err)
		}
		if _, err := zai.Has(ctx, `//div`); err == nil {
			t.Errorf("Has: expected error")
		}
		if _, err := zai.Cookies(ctx); err == nil {
			t.Errorf("Cookies: expected error")
		}
	})
	t.Run("close is nil-safe", func(t *testing.T) {
		if err := (&chromedpBrowser{}).Close(); err != nil {
			t.Errorf("Close on zero browser: %v", err)
		}
		if err := b.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	t.Run("cancelled caller ctx aborts", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		mctx, mcancel := b.withCtx(cctx)
		defer mcancel()
		select {
		case <-mctx.Done():
		default:
			t.Errorf("merged context should be done after caller cancel")
		}
	})
}

// TestChromedpDriverConstruction builds each driver without launching a
// browser (chromedp allocates lazily on first CDP call), then closes it.
func TestChromedpDriverConstruction(t *testing.T) {
	ctx := context.Background()
	t.Run("webshare factory", func(t *testing.T) {
		var f WebshareBrowserFactory = ChromedpWebshareFactory{}
		for _, headless := range []bool{true, false} {
			b, err := f.NewBrowser(ctx, headless)
			if err != nil {
				t.Fatalf("NewBrowser(headless=%v): %v", headless, err)
			}
			if b == nil {
				t.Fatalf("NewBrowser(headless=%v) returned nil", headless)
			}
			if err := b.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}
	})
	t.Run("grok factory func type", func(t *testing.T) {
		var f GrokBrowserFactory = NewGrokChromedpDriver
		d, err := f(ctx, GrokBrowserOptions{
			Headless:   true,
			WindowSize: "1920,1080",
			Proxy:      "http://127.0.0.1:8080",
			UserAgent:  "UA",
			ExtraArgs:  []string{"--no-sandbox"},
			Extensions: []string{"/tmp/ext"},
		})
		if err != nil {
			t.Fatalf("NewGrokChromedpDriver: %v", err)
		}
		if d == nil {
			t.Fatalf("nil driver")
		}
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	t.Run("zai constructor", func(t *testing.T) {
		d, err := NewZaiChromedpDriver(ctx, ZaiChromedpOptions{
			Headless:  true,
			Proxy:     "http://127.0.0.1:8080",
			UserAgent: "UA",
			ExtraArgs: []string{"--no-sandbox"},
		})
		if err != nil {
			t.Fatalf("NewZaiChromedpDriver: %v", err)
		}
		if d == nil {
			t.Fatalf("nil driver")
		}
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

// TestChromedpWebshareFactoryFlags verifies the Python-mirrored flag sets.
func TestChromedpWebshareFactoryFlags(t *testing.T) {
	headlessCfg := chromedpLaunchConfig{
		Headless:  true,
		Args:      append([]string{}, WebshareDefaultChromiumArgs...),
		UserAgent: WebshareHeadlessUserAgent,
	}
	headlessCfg.Args = append(headlessCfg.Args, "--window-size=1920,1080")
	flags := headlessCfg.chromiumFlags()
	for _, want := range []string{
		"--headless",
		"--disable-blink-features=AutomationControlled",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--window-size=1920,1080",
		"--user-agent=" + WebshareHeadlessUserAgent,
	} {
		if !hasChromiumFlag(flags, want) {
			t.Errorf("headless webshare flags %v missing %q", flags, want)
		}
	}
	if got := len(WebshareDefaultChromiumArgs); got != 3 {
		t.Errorf("WebshareDefaultChromiumArgs len = %d, want 3 (guard against drift)", got)
	}
}
