package petaniproxy

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---------- fakes ----------

type zaiFakeDriver struct {
	present map[string]bool
	fills   map[string]string
	clicks  []string
	cookies map[string]string
	navURL  string
	closed  bool
}

func (d *zaiFakeDriver) Navigate(_ context.Context, url string) error {
	d.navURL = url
	return nil
}

func (d *zaiFakeDriver) Fill(_ context.Context, xpath, value string) (bool, error) {
	if !d.present[xpath] {
		return false, nil
	}
	if d.fills == nil {
		d.fills = map[string]string{}
	}
	d.fills[xpath] = value
	return true, nil
}

func (d *zaiFakeDriver) Click(_ context.Context, xpath string) (bool, error) {
	if !d.present[xpath] {
		return false, nil
	}
	d.clicks = append(d.clicks, xpath)
	return true, nil
}

func (d *zaiFakeDriver) Has(_ context.Context, xpath string) (bool, error) {
	return d.present[xpath], nil
}

func (d *zaiFakeDriver) Cookies(_ context.Context) (map[string]string, error) {
	return d.cookies, nil
}

func (d *zaiFakeDriver) Close() error {
	d.closed = true
	return nil
}

func zaiAllPresent() map[string]bool {
	return map[string]bool{
		zaiXPathNameInput:  true,
		zaiXPathEmailInput: true,
		zaiXPathPassInput:  true,
		zaiXPathCaptchaBtn: true,
		zaiXPathSubmitBtn:  true,
	}
}

type zaiFakeStore struct {
	saved []ZaiHarvestedAccount
}

func (s *zaiFakeStore) SaveAccount(_ context.Context, a ZaiHarvestedAccount) error {
	s.saved = append(s.saved, a)
	return nil
}

// zaiMailStub serves a mail.tm-compatible API. subjectCode, when non-empty, is
// embedded in the /messages subject; bodyCode, when non-empty, only appears in
// the full message text at /messages/m1.
func zaiMailStub(t *testing.T, subjectCode, bodyCode string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/domains", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"hydra:member":[
			{"domain":"example.com","isActive":true},
			{"domain":"dead.example","isActive":false},
			{"domain":"noflag.example"}
		]}`)
	})
	mux.HandleFunc("/accounts", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token":"tok-abc-123"}`)
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer tok-abc-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sub := "Welcome aboard"
		if subjectCode != "" {
			sub = "Your verification code is " + subjectCode
		}
		fmt.Fprintf(w, `{"hydra:member":[{"id":"m1","subject":%q,"intro":"hello"}]}`, sub)
	})
	mux.HandleFunc("/messages/m1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"text":"use this code to verify: %s thanks"}`, bodyCode)
	})
	return httptest.NewServer(mux)
}

func zaiFarmWithStub(t *testing.T, drv *zaiFakeDriver, subjectCode, bodyCode string) (*ZaiFarm, *zaiFakeStore, *httptest.Server) {
	t.Helper()
	ts := zaiMailStub(t, subjectCode, bodyCode)
	store := &zaiFakeStore{}
	sink := ZaiAccountFileSink{Path: filepath.Join(t.TempDir(), "out", "zai_accounts.txt")}
	farm := NewZaiFarm(drv, store, sink)
	farm.Out = io.Discard
	farm.Sleep = func(time.Duration) {}
	farm.Rand = rand.New(rand.NewSource(42))
	farm.OTPTimeout = 200 * time.Millisecond
	farm.NewMail = func() *ZaiDuckMailService {
		svc := NewZaiDuckMailService()
		svc.Bases = []string{ts.URL}
		svc.Base = ts.URL
		svc.HTTP = ts.Client()
		return svc
	}
	return farm, store, ts
}

// ---------- pure logic: OTP extraction ----------

func TestZaiExtractOTPCode(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		want  string
		found bool
	}{
		{name: "code in subject", text: "Your verification code is 482910", want: "482910", found: true},
		{name: "code in intro", text: "hello use 123456 to verify", want: "123456", found: true},
		{name: "code in body text", text: "Your Z.ai code: 654321. Expires soon.", want: "654321", found: true},
		{name: "first code wins", text: "111111 then 222222", want: "111111", found: true},
		{name: "seven digits rejected", text: "code 1234567 here", want: "", found: false},
		{name: "five digits rejected", text: "code 12345 here", want: "", found: false},
		{name: "no code", text: "welcome to z.ai, enjoy", want: "", found: false},
		{name: "empty", text: "", want: "", found: false},
		{name: "code glued to letters rejected", text: "abc123456def", want: "", found: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ZaiExtractOTPCode(tt.text)
			if ok != tt.found {
				t.Fatalf("ZaiExtractOTPCode(%q) found = %v, want %v", tt.text, ok, tt.found)
			}
			if got != tt.want {
				t.Errorf("ZaiExtractOTPCode(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// ---------- pure logic: random generators ----------

func TestZaiRandomUser(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	u := ZaiRandomUser(rng)
	if !strings.HasPrefix(u, "zai_") {
		t.Errorf("ZaiRandomUser() = %q, want prefix zai_", u)
	}
	if len(u) != 13 {
		t.Errorf("len(ZaiRandomUser()) = %d, want 13", len(u))
	}
	if ok, _ := regexp.MatchString(`^zai_[a-z0-9]{9}$`, u); !ok {
		t.Errorf("ZaiRandomUser() = %q, want zai_ + 9 [a-z0-9]", u)
	}
}

func TestZaiRandomPassword(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	p := ZaiRandomPassword(rng)
	if ok, _ := regexp.MatchString(`^ZaiFarm[a-zA-Z]{8}!@$`, p); !ok {
		t.Errorf("ZaiRandomPassword() = %q, want ZaiFarm + 8 letters + !@", p)
	}
}

func TestZaiDisplayName(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		n := ZaiDisplayName(rng)
		if ok, _ := regexp.MatchString(`^Zai User \d{3}$`, n); !ok {
			t.Fatalf("ZaiDisplayName() = %q, want 'Zai User NNN'", n)
		}
	}
}

// ---------- pure logic: token selection ----------

func TestZaiSelectTokens(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	genRe := regexp.MustCompile(`^Bearer zai_\d{6}$`)
	tests := []struct {
		name         string
		cookies      map[string]string
		wantAccess   string // empty means "generated placeholder"
		wantRefresh  string // empty means "equals access"
		wantGenMatch bool
	}{
		{name: "token cookie", cookies: map[string]string{"token": "tok1"}, wantAccess: "tok1"},
		{name: "jwt cookie", cookies: map[string]string{"jwt": "jwt1"}, wantAccess: "jwt1"},
		{name: "access_token cookie", cookies: map[string]string{"access_token": "at1"}, wantAccess: "at1"},
		{
			name:       "token wins over jwt",
			cookies:    map[string]string{"token": "tok1", "jwt": "jwt1", "access_token": "at1"},
			wantAccess: "tok1",
		},
		{name: "generated fallback", cookies: map[string]string{}, wantGenMatch: true},
		{
			name:        "refresh cookie kept",
			cookies:     map[string]string{"token": "tok1", "refresh_token": "ref1"},
			wantAccess:  "tok1",
			wantRefresh: "ref1",
		},
		{name: "refresh falls back to access", cookies: map[string]string{"jwt": "jwt9"}, wantAccess: "jwt9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			access, refresh := ZaiSelectTokens(tt.cookies, rng)
			if tt.wantGenMatch {
				if !genRe.MatchString(access) {
					t.Errorf("generated access = %q, want Bearer zai_<6 digits>", access)
				}
			} else if access != tt.wantAccess {
				t.Errorf("access = %q, want %q", access, tt.wantAccess)
			}
			wantRefresh := tt.wantRefresh
			if wantRefresh == "" {
				wantRefresh = access
			}
			if refresh != wantRefresh {
				t.Errorf("refresh = %q, want %q", refresh, wantRefresh)
			}
		})
	}
}

// ---------- pure logic: DB paths ----------

func TestZaiAutoClawDBPaths(t *testing.T) {
	t.Run("env override first", func(t *testing.T) {
		t.Setenv("AUTOCLAWPI_DB", "/tmp/custom.db")
		paths := ZaiAutoClawDBPaths("/mod/root")
		if len(paths) == 0 || paths[0] != "/tmp/custom.db" {
			t.Fatalf("paths[0] = %v, want /tmp/custom.db first", paths)
		}
	})
	t.Run("order without env", func(t *testing.T) {
		t.Setenv("AUTOCLAWPI_DB", "")
		// os.Getenv returns "" for empty; ensure truly unset
		paths := ZaiAutoClawDBPaths("/mod/root")
		home, _ := os.UserHomeDir()
		want := []string{
			filepath.Join(home, ".autoclawpi", "autoclawpi.db"),
			filepath.Join("/mod/root", "output", "autoclawpi.db"),
			filepath.Join("/mod", "autoclawpi", "data", "autoclawpi.db"),
		}
		if len(paths) != len(want) {
			t.Fatalf("len(paths) = %d, want %d (%v)", len(paths), len(want), paths)
		}
		for i := range want {
			if paths[i] != want[i] {
				t.Errorf("paths[%d] = %q, want %q", i, paths[i], want[i])
			}
		}
	})
}

// ---------- mailbox service (httptest-driven) ----------

func TestZaiDuckMailCreateMailbox(t *testing.T) {
	ts := zaiMailStub(t, "", "")
	defer ts.Close()

	svc := NewZaiDuckMailService()
	svc.Bases = []string{ts.URL}
	svc.HTTP = ts.Client()

	email, password, err := svc.CreateMailbox(context.Background())
	if err != nil {
		t.Fatalf("CreateMailbox() error = %v", err)
	}
	if !strings.HasSuffix(email, "@example.com") && !strings.HasSuffix(email, "@noflag.example") {
		t.Errorf("email = %q, want an active stub domain", email)
	}
	if strings.HasSuffix(email, "@dead.example") {
		t.Errorf("email = %q, picked an inactive domain", email)
	}
	if !strings.HasPrefix(email, "zai_") {
		t.Errorf("email = %q, want zai_ prefix", email)
	}
	if ok, _ := regexp.MatchString(`^ZaiFarm[a-zA-Z]{8}!@$`, password); !ok {
		t.Errorf("password = %q, bad format", password)
	}
	if svc.Token != "tok-abc-123" {
		t.Errorf("svc.Token = %q, want tok-abc-123", svc.Token)
	}
	if svc.Base != ts.URL {
		t.Errorf("svc.Base = %q, want stub URL", svc.Base)
	}
}

func TestZaiDuckMailCreateMailboxBaseFailover(t *testing.T) {
	ts := zaiMailStub(t, "", "")
	defer ts.Close()

	svc := NewZaiDuckMailService()
	svc.Bases = []string{"http://127.0.0.1:1", ts.URL} // first base refused
	svc.HTTP = ts.Client()

	email, _, err := svc.CreateMailbox(context.Background())
	if err != nil {
		t.Fatalf("CreateMailbox() error = %v", err)
	}
	if svc.Base != ts.URL {
		t.Errorf("svc.Base = %q, want failover to stub URL", svc.Base)
	}
	if !strings.Contains(email, "@") {
		t.Errorf("email = %q, want a real address", email)
	}
}

func TestZaiDuckMailCreateMailboxFallbackDomain(t *testing.T) {
	svc := NewZaiDuckMailService()
	svc.Bases = []string{"http://127.0.0.1:1", "http://127.0.0.1:2"}
	svc.HTTP = &http.Client{Timeout: 2 * time.Second}

	email, password, err := svc.CreateMailbox(context.Background())
	if err != nil {
		t.Fatalf("CreateMailbox() error = %v", err)
	}
	if !strings.HasSuffix(email, "@"+ZaiFallbackMailDomain) {
		t.Errorf("email = %q, want fallback domain %s", email, ZaiFallbackMailDomain)
	}
	if password == "" {
		t.Error("password empty on fallback")
	}
	if svc.Token != "" {
		t.Errorf("svc.Token = %q, want empty on fallback", svc.Token)
	}
}

func TestZaiDuckMailWaitForOTP(t *testing.T) {
	t.Run("code in subject", func(t *testing.T) {
		ts := zaiMailStub(t, "482910", "")
		defer ts.Close()
		svc := NewZaiDuckMailService()
		svc.Bases = []string{ts.URL}
		svc.HTTP = ts.Client()
		if _, _, err := svc.CreateMailbox(context.Background()); err != nil {
			t.Fatalf("CreateMailbox() error = %v", err)
		}
		code, err := svc.WaitForOTP(context.Background(), 5*time.Second)
		if err != nil {
			t.Fatalf("WaitForOTP() error = %v", err)
		}
		if code != "482910" {
			t.Errorf("code = %q, want 482910", code)
		}
	})

	t.Run("code in message body", func(t *testing.T) {
		ts := zaiMailStub(t, "", "445566")
		defer ts.Close()
		svc := NewZaiDuckMailService()
		svc.Bases = []string{ts.URL}
		svc.HTTP = ts.Client()
		if _, _, err := svc.CreateMailbox(context.Background()); err != nil {
			t.Fatalf("CreateMailbox() error = %v", err)
		}
		code, err := svc.WaitForOTP(context.Background(), 5*time.Second)
		if err != nil {
			t.Fatalf("WaitForOTP() error = %v", err)
		}
		if code != "445566" {
			t.Errorf("code = %q, want 445566", code)
		}
	})

	t.Run("no token returns empty", func(t *testing.T) {
		svc := NewZaiDuckMailService()
		code, err := svc.WaitForOTP(context.Background(), time.Second)
		if err != nil || code != "" {
			t.Errorf("WaitForOTP() = %q, %v; want empty, nil", code, err)
		}
	})

	t.Run("timeout returns empty", func(t *testing.T) {
		ts := zaiMailStub(t, "", "")
		defer ts.Close()
		svc := NewZaiDuckMailService()
		svc.Bases = []string{ts.URL}
		svc.Base = ts.URL
		svc.Token = "tok-abc-123"
		svc.HTTP = ts.Client()
		start := time.Now()
		code, err := svc.WaitForOTP(context.Background(), 60*time.Millisecond)
		if err != nil || code != "" {
			t.Errorf("WaitForOTP() = %q, %v; want empty, nil", code, err)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("WaitForOTP took %v, way past timeout", time.Since(start))
		}
	})
}

// ---------- farm state / logging ----------

func TestZaiFarmLogRingBuffer(t *testing.T) {
	farm := NewZaiFarm(nil, nil, ZaiAccountFileSink{})
	farm.Out = io.Discard
	for i := 0; i < 65; i++ {
		farm.log(ZaiLogInfo, fmt.Sprintf("msg-%d", i), "")
	}
	snap := farm.Snapshot()
	if len(snap.Logs) != 60 {
		t.Fatalf("len(Logs) = %d, want 60", len(snap.Logs))
	}
	if !strings.Contains(snap.Logs[0], "msg-5") {
		t.Errorf("Logs[0] = %q, want oldest kept to be msg-5", snap.Logs[0])
	}
	if !strings.Contains(snap.Logs[59], "msg-64") {
		t.Errorf("Logs[59] = %q, want msg-64", snap.Logs[59])
	}

	farm.log(ZaiLogWarning, "step change", "waiting_otp")
	if got := farm.Snapshot().CurrentStep; got != "waiting_otp" {
		t.Errorf("CurrentStep = %q, want waiting_otp", got)
	}
}

func TestZaiFarmSnapshotIsolation(t *testing.T) {
	farm := NewZaiFarm(nil, nil, ZaiAccountFileSink{})
	farm.Out = io.Discard
	farm.log(ZaiLogInfo, "one", "")
	snap := farm.Snapshot()
	snap.Logs[0] = "mutated"
	if got := farm.Snapshot().Logs[0]; strings.Contains(got, "mutated") {
		t.Errorf("Snapshot shares backing array; Logs[0] = %q", got)
	}
}

// ---------- orchestration (fake driver) ----------

func TestZaiFarmRunSuccess(t *testing.T) {
	drv := &zaiFakeDriver{present: zaiAllPresent(), cookies: map[string]string{"token": "cookie-tok"}}
	farm, store, ts := zaiFarmWithStub(t, drv, "", "")
	defer ts.Close()

	harvested, err := farm.Run(context.Background(), 2, true)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(harvested) != 2 {
		t.Fatalf("len(harvested) = %d, want 2", len(harvested))
	}

	snap := farm.Snapshot()
	if snap.Status != "completed" {
		t.Errorf("Status = %q, want completed", snap.Status)
	}
	if snap.Progress != 100 {
		t.Errorf("Progress = %d, want 100", snap.Progress)
	}
	if snap.HarvestedCount != 2 || len(snap.HarvestedAccounts) != 2 {
		t.Errorf("HarvestedCount = %d, want 2", snap.HarvestedCount)
	}
	if snap.CurrentStep != "completed" {
		t.Errorf("CurrentStep = %q, want completed", snap.CurrentStep)
	}

	if drv.navURL != ZaiAuthURL {
		t.Errorf("navigated to %q, want %s", drv.navURL, ZaiAuthURL)
	}
	if !drv.closed {
		t.Error("driver not closed")
	}
	for _, xp := range []string{zaiXPathNameInput, zaiXPathEmailInput, zaiXPathPassInput} {
		if _, ok := drv.fills[xp]; !ok {
			t.Errorf("no fill recorded for %s", xp)
		}
	}
	clicked := strings.Join(drv.clicks, " ")
	if !strings.Contains(clicked, zaiXPathCaptchaBtn) {
		t.Error("captcha button not clicked")
	}
	if !strings.Contains(clicked, zaiXPathSubmitBtn) {
		t.Error("submit button not clicked")
	}

	if len(store.saved) != 2 {
		t.Fatalf("store.saved = %d, want 2", len(store.saved))
	}
	a := store.saved[0]
	if !strings.HasPrefix(a.Name, "Zai User ") {
		t.Errorf("Name = %q", a.Name)
	}
	if a.AccessToken != "cookie-tok" || a.RefreshToken != "cookie-tok" {
		t.Errorf("tokens = %q/%q, want cookie-tok/cookie-tok", a.AccessToken, a.RefreshToken)
	}

	// File sink: one "email:password:access_token" line per account.
	raw, err := os.ReadFile(farm.Sink.Path)
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("sink lines = %d, want 2", len(lines))
	}
	parts := strings.SplitN(lines[0], ":", 3)
	if len(parts) != 3 || parts[0] != a.Email || parts[1] != a.Password || parts[2] != a.AccessToken {
		t.Errorf("sink line = %q, want email:password:token", lines[0])
	}
}

func TestZaiFarmRunOTPFlow(t *testing.T) {
	present := zaiAllPresent()
	present[zaiXPathOTPInput] = true
	present[zaiXPathOTPConfirm] = true
	drv := &zaiFakeDriver{present: present, cookies: map[string]string{}}
	farm, _, ts := zaiFarmWithStub(t, drv, "482910", "")
	defer ts.Close()

	harvested, err := farm.Run(context.Background(), 1, true)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(harvested) != 1 {
		t.Fatalf("len(harvested) = %d, want 1", len(harvested))
	}
	if got := drv.fills[zaiXPathOTPInput]; got != "482910" {
		t.Errorf("OTP filled = %q, want 482910", got)
	}
	if !strings.Contains(strings.Join(drv.clicks, " "), zaiXPathOTPConfirm) {
		t.Error("OTP confirm button not clicked")
	}
	if farm.Snapshot().Status != "completed" {
		t.Errorf("Status = %q, want completed", farm.Snapshot().Status)
	}
}

func TestZaiFarmRunGeneratedTokenFallback(t *testing.T) {
	drv := &zaiFakeDriver{present: zaiAllPresent(), cookies: map[string]string{}}
	farm, _, ts := zaiFarmWithStub(t, drv, "", "")
	defer ts.Close()

	harvested, err := farm.Run(context.Background(), 1, true)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	a := harvested[0]
	if ok, _ := regexp.MatchString(`^Bearer zai_\d{6}$`, a.AccessToken); !ok {
		t.Errorf("AccessToken = %q, want Bearer zai_<6 digits>", a.AccessToken)
	}
	if a.RefreshToken != a.AccessToken {
		t.Errorf("RefreshToken = %q, want equal to access %q", a.RefreshToken, a.AccessToken)
	}
}

func TestZaiFarmRunNilDriver(t *testing.T) {
	farm := NewZaiFarm(nil, nil, ZaiAccountFileSink{Path: filepath.Join(t.TempDir(), "z.txt")})
	farm.Out = io.Discard
	farm.Sleep = func(time.Duration) {}

	harvested, err := farm.Run(context.Background(), 1, true)
	if err == nil {
		t.Fatal("Run() with nil driver: want error, got nil")
	}
	if harvested != nil {
		t.Errorf("harvested = %v, want nil", harvested)
	}
	snap := farm.Snapshot()
	if snap.Status != "error" {
		t.Errorf("Status = %q, want error", snap.Status)
	}
	if snap.LastError == "" {
		t.Error("LastError empty, want the init failure recorded")
	}
}

func TestZaiFarmRunContextCancel(t *testing.T) {
	drv := &zaiFakeDriver{present: zaiAllPresent(), cookies: map[string]string{}}
	farm, _, ts := zaiFarmWithStub(t, drv, "", "")
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := farm.Run(ctx, 3, true)
	if err == nil {
		t.Fatal("Run() with cancelled context: want error, got nil")
	}
	if got := farm.Snapshot().Status; got != "error" {
		t.Errorf("Status = %q, want error", got)
	}
}
