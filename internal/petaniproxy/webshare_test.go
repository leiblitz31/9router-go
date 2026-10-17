package petaniproxy

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	randv1 "math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeWebshareSleeper struct {
	sleeps []time.Duration
}

func (f *fakeWebshareSleeper) Sleep(d time.Duration) { f.sleeps = append(f.sleeps, d) }

func (f *fakeWebshareSleeper) slept(d time.Duration) int {
	n := 0
	for _, s := range f.sleeps {
		if s == d {
			n++
		}
	}
	return n
}

type fakeWebshareElement struct {
	rect      WebshareRect
	hasRect   bool
	clicks    int
	cleared   int
	typed     []string
	clickErr  error
	typeErr   error
	clickHook func()
}

func (e *fakeWebshareElement) Click(ctx context.Context) error {
	e.clicks++
	if e.clickHook != nil {
		e.clickHook()
	}
	return e.clickErr
}

func (e *fakeWebshareElement) Rect(ctx context.Context) (WebshareRect, bool) {
	return e.rect, e.hasRect
}

func (e *fakeWebshareElement) Clear(ctx context.Context) error {
	e.cleared++
	return nil
}

func (e *fakeWebshareElement) TypeChar(ctx context.Context, ch string) error {
	if e.typeErr != nil {
		return e.typeErr
	}
	e.typed = append(e.typed, ch)
	return nil
}

func (e *fakeWebshareElement) typedText() string { return strings.Join(e.typed, "") }

type fakeWebshareBrowser struct {
	urlCalls      int
	urlFunc       func(calls int) string
	evalFunc      func(script string) (any, error)
	frameEvalFunc func(frameID, script string) (any, error)
	frames        []string
	elements      map[string]*fakeWebshareElement
	mouseMoves    [][2]int
	closed        bool
	navigated     []string
}

func newFakeWebshareBrowser() *fakeWebshareBrowser {
	return &fakeWebshareBrowser{elements: make(map[string]*fakeWebshareElement)}
}

func (b *fakeWebshareBrowser) Navigate(ctx context.Context, url string) error {
	b.navigated = append(b.navigated, url)
	return nil
}

func (b *fakeWebshareBrowser) URL(ctx context.Context) (string, error) {
	b.urlCalls++
	if b.urlFunc != nil {
		return b.urlFunc(b.urlCalls), nil
	}
	return "", nil
}

func (b *fakeWebshareBrowser) Eval(ctx context.Context, script string) (any, error) {
	if b.evalFunc != nil {
		return b.evalFunc(script)
	}
	return nil, nil
}

func (b *fakeWebshareBrowser) EvalInFrame(ctx context.Context, frameID, script string) (any, error) {
	if b.frameEvalFunc != nil {
		return b.frameEvalFunc(frameID, script)
	}
	return nil, nil
}

func (b *fakeWebshareBrowser) Frames(ctx context.Context) ([]string, error) {
	return b.frames, nil
}

func (b *fakeWebshareBrowser) DispatchMouseMove(ctx context.Context, x, y int) error {
	b.mouseMoves = append(b.mouseMoves, [2]int{x, y})
	return nil
}

func (b *fakeWebshareBrowser) FindElement(ctx context.Context, selector string) (WebshareElement, error) {
	if el, ok := b.elements[selector]; ok {
		return el, nil
	}
	return nil, ErrWebshareElementNotFound
}

func (b *fakeWebshareBrowser) Close() error {
	b.closed = true
	return nil
}

type fakeWebshareSolver struct {
	transcript WebshareAudioTranscript
	err        error
	calls      int
	gotAudio   [][]byte
}

func (f *fakeWebshareSolver) SolveAudio(ctx context.Context, audioMP3 []byte) (WebshareAudioTranscript, error) {
	f.calls++
	f.gotAudio = append(f.gotAudio, audioMP3)
	return f.transcript, f.err
}

type fakeWebshareBrowserFactory struct {
	browsers     []*fakeWebshareBrowser
	headlessSeen []bool
	make         func() *fakeWebshareBrowser
}

func (f *fakeWebshareBrowserFactory) NewBrowser(ctx context.Context, headless bool) (WebshareBrowser, error) {
	f.headlessSeen = append(f.headlessSeen, headless)
	b := f.make()
	f.browsers = append(f.browsers, b)
	return b, nil
}

// extractWebshareAppendedChar pulls the typed char back out of the
// WebshareAudioCharJS snippet (undoing the backslash/quote escaping).
func extractWebshareAppendedChar(script string) string {
	const marker = `inp.value += "`
	i := strings.Index(script, marker)
	if i < 0 {
		return ""
	}
	rest := script[i+len(marker):]
	var sb strings.Builder
	escaped := false
	for _, r := range rest {
		switch {
		case escaped:
			sb.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			return sb.String()
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Pure math: Bezier + mouse path + jitter
// ---------------------------------------------------------------------------

func TestWebshareQuadraticBezier(t *testing.T) {
	tests := []struct {
		name          string
		t, p0, p1, p2 float64
		want          float64
	}{
		{"t=0 returns start", 0, 100, 200, 400, 100},
		{"t=1 returns end", 1, 100, 200, 400, 400},
		{"midpoint symmetric", 0.5, 0, 50, 100, 50},
		{"midpoint asymmetric", 0.5, 100, 200, 400, 225},
		{"quarter point", 0.25, 0, 0, 100, 6.25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WebshareQuadraticBezier(tt.t, tt.p0, tt.p1, tt.p2); math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("WebshareQuadraticBezier() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWebshareMousePath(t *testing.T) {
	pts := WebshareMousePath(10, 20, 60, 80, 110, 120, 25)
	if len(pts) != 26 {
		t.Fatalf("len = %d, want 26 (steps+1)", len(pts))
	}
	if pts[0] != (WebsharePoint{X: 10, Y: 20}) {
		t.Errorf("first point = %+v, want start", pts[0])
	}
	if pts[25] != (WebsharePoint{X: 110, Y: 120}) {
		t.Errorf("last point = %+v, want end", pts[25])
	}
	// midpoint of the path must equal the Bezier at t=0.5
	mid := pts[12]
	// t = 12/25 = 0.48
	wantX := WebshareQuadraticBezier(12.0/25.0, 10, 60, 110)
	if math.Abs(mid.X-wantX) > 1e-9 {
		t.Errorf("mid.X = %v, want %v", mid.X, wantX)
	}

	// steps=0 is guarded to a single segment, never NaN
	guarded := WebshareMousePath(0, 0, 5, 5, 10, 10, 0)
	if len(guarded) != 2 {
		t.Fatalf("guarded len = %d, want 2", len(guarded))
	}
	for _, p := range guarded {
		if math.IsNaN(p.X) || math.IsNaN(p.Y) {
			t.Fatalf("NaN point in guarded path: %+v", p)
		}
	}
}

func TestWebshareUniformFloat(t *testing.T) {
	r := randv1.New(randv1.NewSource(7))
	for i := 0; i < 1000; i++ {
		v := WebshareUniformFloat(r, -4, 4)
		if v < -4 || v >= 4 {
			t.Fatalf("value %v out of [-4,4)", v)
		}
	}
}

func TestWebshareAudioListenWait(t *testing.T) {
	tests := []struct {
		duration float64
		want     float64
	}{
		{0, 4.0},
		{2.5, 4.0}, // max(4.0, 2.5+1.5)
		{2.6, 4.1}, // max(4.0, 2.6+1.5)
		{10, 11.5},
	}
	for _, tt := range tests {
		if got := WebshareAudioListenWait(tt.duration); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("WebshareAudioListenWait(%v) = %v, want %v", tt.duration, got, tt.want)
		}
	}
}

func TestWebshareAudioCharJS(t *testing.T) {
	tests := []struct {
		ch   string
		want string // expected escaped literal inside inp.value += "..."
	}{
		{"a", `"a"`},
		{"5", `"5"`},
		{`"`, `"\""`},
		{`\`, `"\\"`},
	}
	for _, tt := range tests {
		t.Run("char "+tt.ch, func(t *testing.T) {
			js := WebshareAudioCharJS(tt.ch)
			if !strings.Contains(js, "inp.value += "+tt.want+";") {
				t.Errorf("JS missing escaped char literal %s:\n%s", tt.want, js)
			}
			// round-trip: the fake extractor must recover the original char
			if got := extractWebshareAppendedChar(js); got != tt.ch {
				t.Errorf("round-trip = %q, want %q", got, tt.ch)
			}
			for _, ev := range []string{"keydown", "keypress", "keyup", "new Event('input'"} {
				if !strings.Contains(js, ev) {
					t.Errorf("JS missing event %q", ev)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Credential generation + domain cleaning
// ---------------------------------------------------------------------------

func TestWebshareRandomEmail(t *testing.T) {
	r := randv1.New(randv1.NewSource(42))
	email := WebshareRandomEmail(r, "example.com")
	if !strings.HasPrefix(email, "ws") {
		t.Errorf("email %q missing ws prefix", email)
	}
	if !strings.HasSuffix(email, "@example.com") {
		t.Errorf("email %q missing @example.com suffix", email)
	}
	local := strings.TrimSuffix(strings.TrimPrefix(email, "ws"), "@example.com")
	if len(local) != 10 {
		t.Errorf("local part %q length = %d, want 10", local, len(local))
	}
	for _, c := range local {
		if !strings.ContainsRune(webshareEmailLocalChars, c) {
			t.Errorf("local part %q has out-of-charset rune %q", local, c)
		}
	}
}

func TestWebshareRandomPassword(t *testing.T) {
	r := randv1.New(randv1.NewSource(42))
	pw := WebshareRandomPassword(r)
	if !strings.HasPrefix(pw, "Passw0rd") {
		t.Errorf("password %q missing Passw0rd prefix", pw)
	}
	if !strings.HasSuffix(pw, "@#") {
		t.Errorf("password %q missing @# suffix", pw)
	}
	if len(pw) != 17 {
		t.Errorf("password %q length = %d, want 17", pw, len(pw))
	}
	if !strings.ContainsRune(websharePasswordSpecials, rune(pw[8])) {
		t.Errorf("password %q special char %q not in %q", pw, pw[8], websharePasswordSpecials)
	}
	for _, c := range pw[9:15] {
		if !strings.ContainsRune(websharePasswordMidChars, c) {
			t.Errorf("password %q mid char %q out of charset", pw, c)
		}
	}
}

func TestWebshareCleanDomain(t *testing.T) {
	tests := []struct{ in, want string }{
		{"@x.com", "x.com"},
		{"x.com", "x.com"},
		{"@@x.com", "x.com"},
		{"  @x.com  ", "x.com"},
		{"", ""},
		{"   ", ""},
	}
	for _, tt := range tests {
		if got := WebshareCleanDomain(tt.in); got != tt.want {
			t.Errorf("WebshareCleanDomain(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

func TestParseWebshareProxyURL(t *testing.T) {
	tests := []struct {
		name             string
		in               string
		user, pass, host string
		port             int
		ok               bool
	}{
		{"valid", "http://user1:pass1@10.0.0.1:8080", "user1", "pass1", "10.0.0.1", 8080, true},
		{"wrong scheme", "socks5://user1:pass1@10.0.0.1:8080", "", "", "", 0, false},
		{"missing password", "http://user1@10.0.0.1:8080", "", "", "", 0, false},
		{"non-numeric port", "http://u:p@h:abc", "", "", "", 0, false},
		{"empty", "", "", "", "", 0, false},
		{"trailing junk still matches like Python re.match", "http://u:p@h:80xyz", "u", "p", "h", 80, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, pass, host, port, ok := ParseWebshareProxyURL(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if user != tt.user || pass != tt.pass || host != tt.host || port != tt.port {
				t.Errorf("got (%q,%q,%q,%d), want (%q,%q,%q,%d)",
					user, pass, host, port, tt.user, tt.pass, tt.host, tt.port)
			}
		})
	}
}

func TestParseWebshareDownloadLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"valid", "1.2.3.4:8080:alice:s3cret", "http://alice:s3cret@1.2.3.4:8080", true},
		{"three parts", "1.2.3.4:8080:alice", "", false},
		{"five parts", "a:b:c:d:e", "", false},
		{"empty part", "1.2.3.4:8080::x", "", false},
		{"blank", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseWebshareDownloadLine(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("ParseWebshareDownloadLine(%q) = (%q,%v), want (%q,%v)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestFormatWebshareProxy(t *testing.T) {
	if got := FormatWebshareProxy("u", "p", "9.9.9.9", 3128); got != "http://u:p@9.9.9.9:3128" {
		t.Errorf("got %q", got)
	}
}

func TestParseWebshareProxyTable(t *testing.T) {
	rows := [][]string{
		{"#", "IP", "Port", "User", "Pass"},       // header: skipped
		{"1", "10.0.0.1", "8080", "alice", "pw1"}, // valid
		{"2", "10.0.0.2", "8080", "bob", "pw2"},   // valid
		{"3", "not-an-ip", "8080", "x", "y"},      // bad ip
		{"4", "10.0.0.4", "notaport", "x", "y"},   // bad port
		{"5", "10.0.0.5", "8080", "", "y"},        // empty user
		{"short", "10.0.0.6"},                     // <5 cells
		{},                                        // empty
	}
	got := ParseWebshareProxyTable(rows)
	want := []string{
		"http://alice:pw1@10.0.0.1:8080",
		"http://bob:pw2@10.0.0.2:8080",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWebshareJSConversions(t *testing.T) {
	if b, ok := WebshareJSBool(true); !ok || !b {
		t.Error("WebshareJSBool(true) failed")
	}
	if _, ok := WebshareJSBool("x"); ok {
		t.Error("WebshareJSBool(string) should fail")
	}
	if s, ok := WebshareJSString("hi"); !ok || s != "hi" {
		t.Error("WebshareJSString failed")
	}
	if _, ok := WebshareJSString(42); ok {
		t.Error("WebshareJSString(int) should fail")
	}
	if ss, ok := WebshareJSStrings([]any{"a", "b"}); !ok || len(ss) != 2 || ss[0] != "a" {
		t.Error("WebshareJSStrings failed")
	}
	if _, ok := WebshareJSStrings([]any{"a", 1}); ok {
		t.Error("WebshareJSStrings mixed should fail")
	}
	if _, ok := WebshareJSStrings("nope"); ok {
		t.Error("WebshareJSStrings(string) should fail")
	}
	rows, ok := WebshareJSRows([]any{[]any{"a", "b"}, []any{"c"}})
	if !ok || len(rows) != 2 || rows[0][1] != "b" {
		t.Error("WebshareJSRows failed")
	}
	if _, ok := WebshareJSRows([]any{[]any{"a", 1}}); ok {
		t.Error("WebshareJSRows mixed should fail")
	}
	if _, ok := WebshareJSRows([]any{"x"}); ok {
		t.Error("WebshareJSRows flat should fail")
	}
}

// ---------------------------------------------------------------------------
// Login monitor state machine
// ---------------------------------------------------------------------------

func TestWebshareLoginMonitorNext(t *testing.T) {
	t0 := time.Now()
	newMon := func() *WebshareLoginMonitor {
		return NewWebshareLoginMonitor(t0, 180*time.Second, 15*time.Second)
	}
	tests := []struct {
		name    string
		elapsed time.Duration
		url     string
		tooMany bool
		want    WebshareMonitorDecision
	}{
		{"timed out", 200 * time.Second, "https://proxy.webshare.io/register", false, WebshareMonitorTimedOut},
		{"timeout beats login", 200 * time.Second, "https://dashboard.webshare.io/", false, WebshareMonitorTimedOut},
		{"logged in", 10 * time.Second, "https://dashboard.webshare.io/", false, WebshareMonitorLoggedIn},
		{"logged in on proxy list", 10 * time.Second, "https://dashboard.webshare.io/proxy/list", false, WebshareMonitorLoggedIn},
		{"register wins over dashboard substring", 10 * time.Second, "https://proxy.webshare.io/register?next=dashboard.webshare.io", false, WebshareMonitorSolve},
		{"too many attempts", 10 * time.Second, "https://proxy.webshare.io/register", true, WebshareMonitorCooldownRetry},
		{"first poll solves", 0, "https://proxy.webshare.io/register", false, WebshareMonitorSolve},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mon := newMon()
			if got := mon.Next(t0.Add(tt.elapsed), tt.url, tt.tooMany); got != tt.want {
				t.Errorf("Next() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWebshareLoginMonitorSolveThrottle(t *testing.T) {
	t0 := time.Now()
	mon := NewWebshareLoginMonitor(t0, 180*time.Second, 15*time.Second)
	url := "https://proxy.webshare.io/register"
	if got := mon.Next(t0, url, false); got != WebshareMonitorSolve {
		t.Fatalf("first Next = %v, want Solve", got)
	}
	if got := mon.Next(t0.Add(5*time.Second), url, false); got != WebshareMonitorKeepWaiting {
		t.Fatalf("5s later = %v, want KeepWaiting", got)
	}
	if got := mon.Next(t0.Add(16*time.Second), url, false); got != WebshareMonitorSolve {
		t.Fatalf("16s later = %v, want Solve", got)
	}
	if got := mon.Next(t0.Add(20*time.Second), url, false); got != WebshareMonitorKeepWaiting {
		t.Fatalf("4s after second solve = %v, want KeepWaiting", got)
	}
}

// ---------------------------------------------------------------------------
// Harvest decision state machine
// ---------------------------------------------------------------------------

func TestDecideWebshareHarvest(t *testing.T) {
	tests := []struct {
		name   string
		signal WebshareHarvestSignal
		want   WebshareHarvestDecision
	}{
		{
			"download link wins over table",
			WebshareHarvestSignal{PageURL: "https://dashboard.webshare.io/proxy/list", DownloadURL: "https://x/download/1", TableProxies: []string{"http://a:b@1.1.1.1:1"}},
			WebshareHarvestFetchDownload,
		},
		{
			"table used when no link",
			WebshareHarvestSignal{PageURL: "https://dashboard.webshare.io/proxy/list", TableProxies: []string{"http://a:b@1.1.1.1:1"}},
			WebshareHarvestUseTable,
		},
		{
			"download button clicked when no link",
			WebshareHarvestSignal{PageURL: "https://dashboard.webshare.io/proxy/list", HasDownloadButton: true},
			WebshareHarvestClickDownload,
		},
		{
			"navigate when off proxy list",
			WebshareHarvestSignal{PageURL: "https://dashboard.webshare.io/", HasNavButton: true},
			WebshareHarvestClickNav,
		},
		{
			"wait when on proxy list with nothing",
			WebshareHarvestSignal{PageURL: "https://dashboard.webshare.io/proxy/list"},
			WebshareHarvestWait,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideWebshareHarvest(tt.signal); got != tt.want {
				t.Errorf("DecideWebshareHarvest() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// File + key + domain helpers
// ---------------------------------------------------------------------------

func TestAppendUniqueProxyLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "proxies.txt")
	added, total, err := AppendUniqueProxyLines(path, []string{"http://a:b@1.1.1.1:1", "http://c:d@2.2.2.2:2", ""})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if added != 2 || total != 2 {
		t.Errorf("added=%d total=%d, want 2/2", added, total)
	}
	added, total, err = AppendUniqueProxyLines(path, []string{"http://c:d@2.2.2.2:2", "http://e:f@3.3.3.3:3"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if added != 1 || total != 3 {
		t.Errorf("added=%d total=%d, want 1/3", added, total)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Errorf("file has %d lines, want 3: %q", len(lines), data)
	}
}

func TestLookupCapsolverKey(t *testing.T) {
	t.Setenv("CAPSOLVER_API_KEY", "")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "config"), 0o755)
	os.WriteFile(filepath.Join(dir, "config", "settings.json"), []byte(`{"capsolver_api_key": "json-key-1"}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("CAPSOLVER_API_KEY=dotenv-key\n"), 0o644)
	// config/settings.json wins over .env (Python probes it first)
	if got := LookupCapsolverKey([]string{dir}); got != "json-key-1" {
		t.Errorf("got %q, want json-key-1", got)
	}
	// env wins over everything
	t.Setenv("CAPSOLVER_API_KEY", "env-key")
	if got := LookupCapsolverKey([]string{dir}); got != "env-key" {
		t.Errorf("got %q, want env-key", got)
	}
	t.Setenv("CAPSOLVER_API_KEY", "")
	// .env fallback when no json key
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, ".env"), []byte("CAPSOLVER_API_KEY=\"quoted-key\"\n"), 0o644)
	if got := LookupCapsolverKey([]string{dir2}); got != "quoted-key" {
		t.Errorf("got %q, want quoted-key", got)
	}
	// nothing configured
	if got := LookupCapsolverKey([]string{t.TempDir()}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestCheckCapsolverBalance(t *testing.T) {
	// empty key: no network at all
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	old := webshareCapsolverAPIBase
	webshareCapsolverAPIBase = srv.URL
	defer func() { webshareCapsolverAPIBase = old }()

	st := CheckCapsolverBalance(context.Background(), srv.Client(), "")
	if st.Status != "NOT_CONFIGURED" || st.HasKey || st.CanHeadless {
		t.Errorf("empty key: %+v", st)
	}
	if st.Message != "Key Kosong (Free Audio Solver Aktif)" {
		t.Errorf("message = %q", st.Message)
	}
	if hits != 0 {
		t.Errorf("empty key hit the network %d times", hits)
	}

	// table over live server
	var missingKey atomic.Bool
	balanceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ClientKey string `json:"clientKey"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.ClientKey == "" {
			missingKey.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(balanceSrvBody))
	}))
	defer balanceSrv.Close()
	webshareCapsolverAPIBase = balanceSrv.URL

	tests := []struct {
		name         string
		body         string
		wantStatus   string
		wantHeadless bool
		wantMsg      string
	}{
		{"ready", `{"errorId":0,"balance":1.234}`, "READY", true, "Saldo Aktif: $1.234 (Headless Ready)"},
		{"exact threshold", `{"errorId":0,"balance":0.003}`, "READY", true, "Saldo Aktif: $0.003 (Headless Ready)"},
		{"empty", `{"errorId":0,"balance":0.001}`, "EMPTY", false, "Saldo Habis: $0.001 (Headless Dimatikan)"},
		{"invalid", `{"errorId":1,"errorCode":"WRONG_KEY"}`, "INVALID", false, "Key Tidak Valid (WRONG_KEY)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			balanceSrvBody = tt.body
			st := CheckCapsolverBalance(context.Background(), balanceSrv.Client(), "k")
			if st.Status != tt.wantStatus || st.CanHeadless != tt.wantHeadless || st.Message != tt.wantMsg {
				t.Errorf("got %+v, want status=%s headless=%v msg=%q", st, tt.wantStatus, tt.wantHeadless, tt.wantMsg)
			}
			if !st.HasKey {
				t.Error("HasKey should be true")
			}
		})
	}
	if missingKey.Load() {
		t.Error("clientKey missing from a getBalance request")
	}

	// timeout: server stalls, client gives up fast
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer stall.Close()
	webshareCapsolverAPIBase = stall.URL
	fastClient := stall.Client()
	fastClient.Timeout = 50 * time.Millisecond
	st = CheckCapsolverBalance(context.Background(), fastClient, "k")
	if st.Status != "TIMEOUT" || !st.HasKey || st.CanHeadless {
		t.Errorf("timeout: %+v", st)
	}
}

var balanceSrvBody string

func TestResolveWebshareEmailDomain(t *testing.T) {
	ctx := context.Background()
	pickZero := func(n int) int { return 0 }

	t.Run("custom wins", func(t *testing.T) {
		got := ResolveWebshareEmailDomain(ctx, WebshareDomainOptions{CustomDomain: "@custom.com", Pick: pickZero})
		if got != "custom.com" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("env wins over config", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"custom_email_domain":"cfg.com"}`), 0o644)
		got := ResolveWebshareEmailDomain(ctx, WebshareDomainOptions{EnvDomain: "env.com", ConfigDirs: []string{dir}, Pick: pickZero})
		if got != "env.com" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("config file used", func(t *testing.T) {
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "config"), 0o755)
		os.WriteFile(filepath.Join(dir, "config", "settings.json"), []byte(`{"webshare_email_domain":"@cfgfile.com"}`), 0o644)
		got := ResolveWebshareEmailDomain(ctx, WebshareDomainOptions{ConfigDirs: []string{dir}, Pick: pickZero})
		if got != "cfgfile.com" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("duckmail live", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"hydra:member":[{"domain":"live.com","isVerified":true},{"domain":"bad.com","isVerified":false},{"domain":"","isVerified":true}]}`))
		}))
		defer srv.Close()
		got := ResolveWebshareEmailDomain(ctx, WebshareDomainOptions{DuckMailURL: srv.URL, Client: srv.Client(), Pick: pickZero})
		if got != "live.com" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("mailtm fallback when duckmail empty", func(t *testing.T) {
		duck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"hydra:member":[]}`))
		}))
		defer duck.Close()
		mail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"hydra:member":[{"domain":"mtm.com","isActive":true},{"domain":"dead.com","isActive":false}]}`))
		}))
		defer mail.Close()
		got := ResolveWebshareEmailDomain(ctx, WebshareDomainOptions{
			DuckMailURL: duck.URL, MailTmURL: mail.URL, Client: duck.Client(), Pick: pickZero,
		})
		if got != "mtm.com" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("fallback pool", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer dead.Close()
		got := ResolveWebshareEmailDomain(ctx, WebshareDomainOptions{
			DuckMailURL: dead.URL, MailTmURL: dead.URL, Client: dead.Client(), Pick: pickZero,
		})
		if got != WebshareFallbackEmailDomains[0] {
			t.Errorf("got %q, want %q", got, WebshareFallbackEmailDomains[0])
		}
	})
}

// ---------------------------------------------------------------------------
// Store + sync
// ---------------------------------------------------------------------------

func TestWebshareMemoryStore(t *testing.T) {
	ctx := context.Background()
	m := NewWebshareMemoryStore()
	existing, err := m.ExistingProxyURLs(ctx)
	if err != nil || len(existing) != 0 {
		t.Fatalf("fresh store: %v %v", existing, err)
	}
	now := time.Now().UTC()
	m.InsertProxy(ctx, WebshareProxyPoolEntry{ProxyURL: "http://a:b@1.1.1.1:1", Name: "n1", CreatedAt: now})
	m.InsertProxy(ctx, WebshareProxyPoolEntry{ProxyURL: "http://c:d@2.2.2.2:2", Name: "n2", CreatedAt: now})
	existing, _ = m.ExistingProxyURLs(ctx)
	if len(existing) != 2 {
		t.Errorf("existing = %d, want 2", len(existing))
	}
	if len(m.All()) != 2 {
		t.Errorf("All() = %d, want 2", len(m.All()))
	}
}

func TestSyncWebshareToStore(t *testing.T) {
	ctx := context.Background()
	m := NewWebshareMemoryStore()
	m.InsertProxy(ctx, WebshareProxyPoolEntry{ProxyURL: "http://old:pw@9.9.9.9:9"})
	added, err := SyncWebshareToStore(ctx, m, []string{
		"http://old:pw@9.9.9.9:9",    // dup: skipped
		"http://u1:p1@10.0.0.1:8080", // new
		"notaurl",                    // malformed: skipped
		"http://u1:p1@10.0.0.1:8080", // dup within batch: skipped
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if added != 1 {
		t.Errorf("added = %d, want 1", added)
	}
	all := m.All()
	if len(all) != 2 {
		t.Fatalf("stored = %d, want 2", len(all))
	}
	var entry WebshareProxyPoolEntry
	for _, e := range all {
		if e.ProxyURL == "http://u1:p1@10.0.0.1:8080" {
			entry = e
		}
	}
	if entry.Name != "Webshare Resi (10.0.0.1:8080)" {
		t.Errorf("Name = %q", entry.Name)
	}
	if entry.Type != "http" || entry.StrictProxy || entry.ID == "" {
		t.Errorf("entry = %+v", entry)
	}
	if entry.CreatedAt.IsZero() || entry.UpdatedAt.IsZero() {
		t.Error("timestamps not set")
	}
	if entry.LastTestedAt != nil || entry.LastError != nil {
		t.Error("lastTestedAt/lastError should be nil")
	}
}

// ---------------------------------------------------------------------------
// Human behavior orchestration
// ---------------------------------------------------------------------------

func TestSimulateHumanMouse(t *testing.T) {
	ctx := context.Background()
	b := newFakeWebshareBrowser()
	r := randv1.New(randv1.NewSource(3))
	s := &fakeWebshareSleeper{}
	if err := SimulateHumanMouse(ctx, b, 500, 600, 25, r, s); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(b.mouseMoves) != 26 {
		t.Fatalf("moves = %d, want 26", len(b.mouseMoves))
	}
	last := b.mouseMoves[25]
	if last != [2]int{500, 600} {
		t.Errorf("last move = %v, want [500 600]", last)
	}
	first := b.mouseMoves[0]
	if first[0] < 100 || first[0] > 400 || first[1] < 100 || first[1] > 400 {
		t.Errorf("first move = %v, want start in [100,400]^2", first)
	}
	if len(s.sleeps) != 27 { // 26 per-step + 1 trailing
		t.Errorf("sleeps = %d, want 27", len(s.sleeps))
	}
}

func TestHumanClickWebshareElement(t *testing.T) {
	ctx := context.Background()
	r := randv1.New(randv1.NewSource(3))
	s := &fakeWebshareSleeper{}

	b := newFakeWebshareBrowser()
	el := &fakeWebshareElement{rect: WebshareRect{X: 10, Y: 20, W: 100, H: 50}, hasRect: true}
	if err := HumanClickWebshareElement(ctx, b, el, r, s); err != nil {
		t.Fatalf("err = %v", err)
	}
	if el.clicks != 1 {
		t.Errorf("clicks = %d, want 1", el.clicks)
	}
	if len(b.mouseMoves) != 26 {
		t.Fatalf("moves = %d, want 26", len(b.mouseMoves))
	}
	// target = center (60,45) +/- 4 jitter
	last := b.mouseMoves[25]
	if last[0] < 56 || last[0] > 64 || last[1] < 41 || last[1] > 49 {
		t.Errorf("last move = %v, want near [60 45] +/- 4", last)
	}

	// no rect: straight to click
	b2 := newFakeWebshareBrowser()
	el2 := &fakeWebshareElement{}
	if err := HumanClickWebshareElement(ctx, b2, el2, r, s); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(b2.mouseMoves) != 0 || el2.clicks != 1 {
		t.Errorf("moves=%d clicks=%d, want 0/1", len(b2.mouseMoves), el2.clicks)
	}

	// click error propagates
	el3 := &fakeWebshareElement{clickErr: errors.New("boom")}
	if err := HumanClickWebshareElement(ctx, b2, el3, r, s); err == nil {
		t.Error("expected click error")
	}
}

func TestTypeTextHumanly(t *testing.T) {
	ctx := context.Background()
	r := randv1.New(randv1.NewSource(3))
	s := &fakeWebshareSleeper{}
	el := &fakeWebshareElement{}
	if err := TypeTextHumanly(ctx, el, "h\u00e9llo", r, s, 0.02, 0.07); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := el.typedText(); got != "h\u00e9llo" {
		t.Errorf("typed = %q", got)
	}
	if len(el.typed) != 5 { // rune-wise, not byte-wise
		t.Errorf("typed runes = %d, want 5", len(el.typed))
	}
	if len(s.sleeps) != 5 {
		t.Errorf("sleeps = %d, want 5", len(s.sleeps))
	}
	for _, d := range s.sleeps {
		if d < 20*time.Millisecond || d >= 70*time.Millisecond {
			t.Errorf("sleep %v out of [20ms,70ms)", d)
		}
	}
	el2 := &fakeWebshareElement{typeErr: errors.New("nope")}
	if err := TypeTextHumanly(ctx, el2, "ab", r, s, 0.02, 0.07); err == nil {
		t.Error("expected type error")
	}
}

// ---------------------------------------------------------------------------
// Audio captcha orchestration
// ---------------------------------------------------------------------------

func TestWebshareHunterSolveAudioChallenge(t *testing.T) {
	ctx := context.Background()
	mp3 := []byte("fake-mp3-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(mp3)
	}))
	defer srv.Close()

	var appended []string
	var verifyClicked bool
	b := newFakeWebshareBrowser()
	b.frames = []string{"frame1"}
	b.frameEvalFunc = func(frameID, script string) (any, error) {
		switch {
		case strings.Contains(script, `inp.value +=`):
			appended = append(appended, extractWebshareAppendedChar(script))
			return nil, nil
		case strings.Contains(script, "recaptcha-verify-button"):
			verifyClicked = true
			return true, nil
		case strings.Contains(script, "recaptcha-audio-button"):
			return false, nil
		case strings.Contains(script, "audio-source"):
			return srv.URL + "/audio.mp3", nil
		case strings.Contains(script, "automated queries"):
			return "", nil
		case strings.Contains(script, "audio-response"):
			return true, nil
		}
		return nil, nil
	}

	solver := &fakeWebshareSolver{transcript: WebshareAudioTranscript{Text: "7391", DurationSec: 2.5}}
	sleeper := &fakeWebshareSleeper{}
	h := NewWebshareHunter(WebshareHunterConfig{
		Rand:          randv1.New(randv1.NewSource(1)),
		Sleeper:       sleeper,
		CaptchaSolver: solver,
		HTTPClient:    srv.Client(),
	})

	solved, err := h.solveAudioChallenge(ctx, b)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !solved {
		t.Fatal("expected solved=true")
	}
	if solver.calls != 1 {
		t.Errorf("solver calls = %d, want 1", solver.calls)
	}
	if len(solver.gotAudio) != 1 || string(solver.gotAudio[0]) != string(mp3) {
		t.Error("solver did not receive the downloaded mp3 bytes")
	}
	if got := strings.Join(appended, ""); got != "7391" {
		t.Errorf("typed chars = %q, want 7391", got)
	}
	if !verifyClicked {
		t.Error("verify button was not clicked")
	}
	// listen wait = max(4.0, 2.5+1.5) = 4s, plus 0.5s focus pause and 6s post-verify
	if sleeper.slept(4*time.Second) != 1 || sleeper.slept(500*time.Millisecond) != 1 || sleeper.slept(6*time.Second) != 1 {
		t.Errorf("expected 4s/0.5s/6s sleeps, got %v", sleeper.sleeps)
	}
}

func TestWebshareHunterSolveAudioChallengeLimit(t *testing.T) {
	ctx := context.Background()
	b := newFakeWebshareBrowser()
	b.frames = []string{"frame1"}
	b.frameEvalFunc = func(frameID, script string) (any, error) {
		if strings.Contains(script, "automated queries") {
			return "Your computer or network may be sending automated queries", nil
		}
		if strings.Contains(script, "audio-response") {
			return true, nil
		}
		return nil, nil
	}
	h := NewWebshareHunter(WebshareHunterConfig{
		Rand:          randv1.New(randv1.NewSource(1)),
		Sleeper:       &fakeWebshareSleeper{},
		CaptchaSolver: &fakeWebshareSolver{},
	})
	solved, err := h.solveAudioChallenge(ctx, b)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if solved {
		t.Error("rate-limited challenge must not solve")
	}
}

func TestWebshareHunterSolveAudioChallengeSolverError(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	b := newFakeWebshareBrowser()
	b.frames = []string{"frame1"}
	b.frameEvalFunc = func(frameID, script string) (any, error) {
		if strings.Contains(script, "audio-source") {
			return srv.URL + "/a.mp3", nil
		}
		if strings.Contains(script, "audio-response") {
			return true, nil
		}
		if strings.Contains(script, "automated queries") {
			return "", nil
		}
		return nil, nil
	}
	h := NewWebshareHunter(WebshareHunterConfig{
		Rand:          randv1.New(randv1.NewSource(1)),
		Sleeper:       &fakeWebshareSleeper{},
		CaptchaSolver: &fakeWebshareSolver{err: errors.New("stt down")},
		HTTPClient:    srv.Client(),
	})
	solved, err := h.solveAudioChallenge(ctx, b)
	if err != nil || solved {
		t.Errorf("solved=%v err=%v, want false/nil", solved, err)
	}
}

// ---------------------------------------------------------------------------
// CapSolver orchestration
// ---------------------------------------------------------------------------

func TestWebshareHunterSolveWithCapsolver(t *testing.T) {
	ctx := context.Background()
	var resultCalls int
	var badPayload atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/createTask"):
			var req struct {
				Task map[string]string `json:"task"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Task["type"] != "ReCaptchaV2TaskProxyLess" || req.Task["websiteKey"] != "SITEKEY" {
				badPayload.Store(true)
			}
			w.Write([]byte(`{"errorId":0,"taskId":"task-1"}`))
		case strings.HasSuffix(r.URL.Path, "/getTaskResult"):
			resultCalls++
			if resultCalls == 1 {
				w.Write([]byte(`{"status":"processing"}`))
			} else {
				w.Write([]byte(`{"status":"ready","solution":{"gRecaptchaResponse":"TOKEN123"}}`))
			}
		}
	}))
	defer srv.Close()
	old := webshareCapsolverAPIBase
	webshareCapsolverAPIBase = srv.URL
	defer func() { webshareCapsolverAPIBase = old }()

	var injected string
	b := newFakeWebshareBrowser()
	b.evalFunc = func(script string) (any, error) {
		if strings.Contains(script, "data-sitekey") {
			return "SITEKEY", nil
		}
		if strings.Contains(script, "g-recaptcha-response") {
			injected = script
			return nil, nil
		}
		return nil, nil
	}
	b.urlFunc = func(calls int) string { return "https://proxy.webshare.io/register" }

	h := NewWebshareHunter(WebshareHunterConfig{
		Rand:    randv1.New(randv1.NewSource(1)),
		Sleeper: &fakeWebshareSleeper{},
	})
	if !h.solveWithCapsolver(ctx, b, "key") {
		t.Fatal("expected solved=true")
	}
	if !strings.Contains(injected, "TOKEN123") {
		t.Errorf("token not injected: %q", injected)
	}
	if resultCalls != 2 {
		t.Errorf("getTaskResult calls = %d, want 2", resultCalls)
	}
	if badPayload.Load() {
		t.Error("bad createTask payload")
	}
}

func TestWebshareHunterSolveWithCapsolverNoSitekey(t *testing.T) {
	ctx := context.Background()
	b := newFakeWebshareBrowser()
	b.evalFunc = func(script string) (any, error) { return "", nil }
	h := NewWebshareHunter(WebshareHunterConfig{
		Rand:    randv1.New(randv1.NewSource(1)),
		Sleeper: &fakeWebshareSleeper{},
	})
	if h.solveWithCapsolver(ctx, b, "key") {
		t.Error("expected false without sitekey")
	}
}

func TestWebshareHunterSolveWithCapsolverTaskFailed(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/createTask") {
			w.Write([]byte(`{"errorId":0,"taskId":"task-1"}`))
		} else {
			w.Write([]byte(`{"status":"failed"}`))
		}
	}))
	defer srv.Close()
	old := webshareCapsolverAPIBase
	webshareCapsolverAPIBase = srv.URL
	defer func() { webshareCapsolverAPIBase = old }()

	b := newFakeWebshareBrowser()
	b.evalFunc = func(script string) (any, error) { return "SITEKEY", nil }
	h := NewWebshareHunter(WebshareHunterConfig{
		Rand:    randv1.New(randv1.NewSource(1)),
		Sleeper: &fakeWebshareSleeper{},
	})
	if h.solveWithCapsolver(ctx, b, "key") {
		t.Error("expected false on failed task")
	}
}

// ---------------------------------------------------------------------------
// End-to-end hunt with fakes
// ---------------------------------------------------------------------------

// e2eWebshareBrowser builds a fake browser that walks the whole happy path:
// register page -> (audio solve) -> dashboard proxy list -> table harvest.
func e2eWebshareBrowser(t *testing.T, mp3srv *httptest.Server, rows []any) *fakeWebshareBrowser {
	t.Helper()
	b := newFakeWebshareBrowser()
	b.urlFunc = func(calls int) string {
		if calls < 2 {
			return "https://proxy.webshare.io/register"
		}
		return "https://dashboard.webshare.io/proxy/list"
	}
	b.elements["@name=email"] = &fakeWebshareElement{rect: WebshareRect{X: 10, Y: 10, W: 200, H: 30}, hasRect: true}
	b.elements["@name=password"] = &fakeWebshareElement{rect: WebshareRect{X: 10, Y: 50, W: 200, H: 30}, hasRect: true}
	b.elements["tag:input@type=checkbox"] = &fakeWebshareElement{hasRect: false}
	b.elements["text:Sign Up With Email"] = &fakeWebshareElement{hasRect: false}
	b.evalFunc = func(script string) (any, error) {
		switch {
		case strings.Contains(script, "Too many attempts"):
			return false, nil
		case strings.Contains(script, "proxy/list/download"):
			return "", nil
		case strings.Contains(script, "querySelectorAll('tr')"):
			return rows, nil
		}
		return nil, nil
	}
	b.frames = []string{"frame1"}
	b.frameEvalFunc = func(frameID, script string) (any, error) {
		switch {
		case strings.Contains(script, `inp.value +=`):
			return nil, nil
		case strings.Contains(script, "recaptcha-verify-button"):
			return true, nil
		case strings.Contains(script, "audio-source"):
			return mp3srv.URL + "/audio.mp3", nil
		case strings.Contains(script, "automated queries"):
			return "", nil
		case strings.Contains(script, "audio-response"):
			return true, nil
		}
		return nil, nil
	}
	return b
}

func TestWebshareHunterHuntSingleE2E(t *testing.T) {
	t.Setenv("CAPSOLVER_API_KEY", "")
	ctx := context.Background()
	mp3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("mp3"))
	}))
	defer mp3srv.Close()

	rows := []any{
		[]any{"1", "10.0.0.1", "8080", "alice", "pw1"},
		[]any{"2", "10.0.0.2", "8080", "bob", "pw2"},
	}
	factory := &fakeWebshareBrowserFactory{make: func() *fakeWebshareBrowser {
		return e2eWebshareBrowser(t, mp3srv, rows)
	}}
	sleeper := &fakeWebshareSleeper{}
	solver := &fakeWebshareSolver{transcript: WebshareAudioTranscript{Text: "12", DurationSec: 1}}
	h := NewWebshareHunter(WebshareHunterConfig{
		CustomDomain:   "example.com",
		ConfigDirs:     []string{t.TempDir()},
		BrowserFactory: factory,
		CaptchaSolver:  solver,
		Rand:           randv1.New(randv1.NewSource(9)),
		Sleeper:        sleeper,
		HTTPClient:     mp3srv.Client(),
		LoginTimeout:   30 * time.Second,
		HarvestTimeout: 30 * time.Second,
	})

	proxies, err := h.HuntSingle(ctx, 1, 1)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []string{"http://alice:pw1@10.0.0.1:8080", "http://bob:pw2@10.0.0.2:8080"}
	if len(proxies) != len(want) {
		t.Fatalf("proxies = %v", proxies)
	}
	for i := range want {
		if proxies[i] != want[i] {
			t.Errorf("proxies[%d] = %q, want %q", i, proxies[i], want[i])
		}
	}

	b := factory.browsers[0]
	if len(b.navigated) != 1 || b.navigated[0] != WebshareRegisterURL {
		t.Errorf("navigated = %v", b.navigated)
	}
	email := b.elements["@name=email"].typedText()
	if !strings.HasPrefix(email, "ws") || !strings.HasSuffix(email, "@example.com") {
		t.Errorf("email typed = %q", email)
	}
	pw := b.elements["@name=password"].typedText()
	if !strings.HasPrefix(pw, "Passw0rd") || !strings.HasSuffix(pw, "@#") || len(pw) != 17 {
		t.Errorf("password typed = %q", pw)
	}
	if b.elements["text:Sign Up With Email"].clicks == 0 {
		t.Error("signup button never clicked")
	}
	if !b.closed {
		t.Error("browser not closed")
	}
	if len(factory.headlessSeen) != 1 || factory.headlessSeen[0] {
		t.Errorf("headless flags = %v, want [false]", factory.headlessSeen)
	}
	if solver.calls != 1 {
		t.Errorf("solver calls = %d, want 1", solver.calls)
	}
}

func TestWebshareHunterHuntSingleLoginTimeout(t *testing.T) {
	t.Setenv("CAPSOLVER_API_KEY", "")
	ctx := context.Background()
	factory := &fakeWebshareBrowserFactory{make: func() *fakeWebshareBrowser {
		b := newFakeWebshareBrowser()
		b.urlFunc = func(calls int) string { return "https://proxy.webshare.io/register" }
		b.evalFunc = func(script string) (any, error) { return false, nil }
		b.frames = []string{} // no frames: audio solver is a no-op
		return b
	}}
	h := NewWebshareHunter(WebshareHunterConfig{
		CustomDomain:   "example.com",
		ConfigDirs:     []string{t.TempDir()},
		BrowserFactory: factory,
		Rand:           randv1.New(randv1.NewSource(9)),
		Sleeper:        &fakeWebshareSleeper{},
		LoginTimeout:   20 * time.Millisecond,
		HarvestTimeout: time.Second,
	})
	start := time.Now()
	proxies, err := h.HuntSingle(ctx, 1, 1)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(proxies) != 0 {
		t.Errorf("proxies = %v, want empty", proxies)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("hunt did not respect the login timeout")
	}
	if !factory.browsers[0].closed {
		t.Error("browser not closed after timeout")
	}
}

func TestWebshareHunterHuntSingleNoFactory(t *testing.T) {
	h := NewWebshareHunter(WebshareHunterConfig{CustomDomain: "example.com"})
	if _, err := h.HuntSingle(context.Background(), 1, 1); err == nil {
		t.Error("expected error without a BrowserFactory")
	}
}

func TestRunWebshareHunter(t *testing.T) {
	t.Setenv("CAPSOLVER_API_KEY", "")
	ctx := context.Background()
	mp3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("mp3"))
	}))
	defer mp3srv.Close()

	var account int
	factory := &fakeWebshareBrowserFactory{make: func() *fakeWebshareBrowser {
		account++
		ip := "10.1.0.1"
		if account == 2 {
			ip = "10.2.0.2"
		}
		return e2eWebshareBrowser(t, mp3srv, []any{[]any{"1", ip, "8080", "u", "p"}})
	}}
	outDir := t.TempDir()
	grokTxt := filepath.Join(t.TempDir(), "grok", "proxies.txt")
	store := NewWebshareMemoryStore()
	sleeper := &fakeWebshareSleeper{}

	all, err := RunWebshareHunter(ctx, WebshareHunterConfig{
		Total:          2,
		CustomDomain:   "example.com",
		ConfigDirs:     []string{t.TempDir()},
		OutputDir:      outDir,
		ProxiesTxtPath: grokTxt,
		Store:          store,
		BrowserFactory: factory,
		CaptchaSolver:  &fakeWebshareSolver{transcript: WebshareAudioTranscript{Text: "1", DurationSec: 1}},
		Rand:           randv1.New(randv1.NewSource(9)),
		Sleeper:        sleeper,
		HTTPClient:     mp3srv.Client(),
		LoginTimeout:   30 * time.Second,
		HarvestTimeout: 30 * time.Second,
		AccountGap:     7 * time.Second,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %v", all)
	}
	for _, path := range []string{
		filepath.Join(outDir, "webshare_residential.txt"),
		filepath.Join(outDir, "live_elite.txt"),
		grokTxt,
	} {
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != 2 {
			t.Errorf("%s has %d lines, want 2", path, len(lines))
		}
	}
	if len(store.All()) != 2 {
		t.Errorf("store has %d entries, want 2", len(store.All()))
	}
	if sleeper.slept(7*time.Second) != 1 {
		t.Errorf("expected exactly one 7s account gap, got %v", sleeper.sleeps)
	}
	if len(factory.browsers) != 2 {
		t.Errorf("browsers created = %d, want 2", len(factory.browsers))
	}
}

func TestNewWebshareHunterDefaults(t *testing.T) {
	h := NewWebshareHunter(WebshareHunterConfig{})
	if h.cfg.LoginTimeout != WebshareDefaultLoginTimeout {
		t.Errorf("LoginTimeout = %v", h.cfg.LoginTimeout)
	}
	if h.cfg.HarvestTimeout != WebshareDefaultHarvestTimeout {
		t.Errorf("HarvestTimeout = %v", h.cfg.HarvestTimeout)
	}
	if h.cfg.SolverInterval != WebshareDefaultSolverInterval {
		t.Errorf("SolverInterval = %v", h.cfg.SolverInterval)
	}
	if h.cfg.CooldownRetry != WebshareDefaultCooldownRetry {
		t.Errorf("CooldownRetry = %v", h.cfg.CooldownRetry)
	}
	if h.cfg.AccountGap != WebshareDefaultAccountGap {
		t.Errorf("AccountGap = %v", h.cfg.AccountGap)
	}
	if h.cfg.FinalHold != WebshareDefaultFinalHold {
		t.Errorf("FinalHold = %v", h.cfg.FinalHold)
	}
	if h.rand == nil || h.sleeper == nil || h.httpClient == nil || h.solver == nil {
		t.Error("nil default dependency")
	}
	if h.cfg.HeadlessUserAgent != WebshareHeadlessUserAgent {
		t.Errorf("HeadlessUserAgent = %q", h.cfg.HeadlessUserAgent)
	}
	if len(h.cfg.ChromiumArgs) != len(WebshareDefaultChromiumArgs) {
		t.Errorf("ChromiumArgs = %v", h.cfg.ChromiumArgs)
	}
	// explicit values survive
	h2 := NewWebshareHunter(WebshareHunterConfig{LoginTimeout: time.Second, ChromiumArgs: []string{"--x"}})
	if h2.cfg.LoginTimeout != time.Second || len(h2.cfg.ChromiumArgs) != 1 {
		t.Error("explicit config values were overwritten")
	}
}

func TestWebshareStubCaptchaSolver(t *testing.T) {
	var s WebshareCaptchaSolver = WebshareStubCaptchaSolver{}
	if _, err := s.SolveAudio(context.Background(), []byte("x")); err == nil {
		t.Error("stub should always error")
	}
}
