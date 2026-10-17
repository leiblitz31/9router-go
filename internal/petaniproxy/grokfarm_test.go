package petaniproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// noDelays keeps orchestration tests fast (zero Delays would select the
// production defaults instead).
var noDelays = GrokRegisterDelays{
	Open: time.Millisecond, AfterClick: time.Millisecond,
	AfterOTP: time.Millisecond, AfterProfile: time.Millisecond,
}

// ---------------------------------------------------------------------------
// Pure logic: OTP extraction
// ---------------------------------------------------------------------------

func TestExtractGrokOTP(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"dashed alnum", "Your xAI code is 482-916, enter it now", "482916"},
		{"dashed uppercase", "Code: ABC-123", "ABC123"},
		{"plain six digit", "Your verification code is 482916", "482916"},
		{"dashed wins over plain", "482-916 then 123456", "482916"},
		{"no code", "hello world, no digits here", ""},
		{"five digits", "code 12345", ""},
		{"seven digits embedded", "x4829167", ""},
		{"empty", "", ""},
		{"dashed inside longer token", "xx482-916yy", ""},
		{"second dashed pair", "Verify your account 12-34-56 987-654", "987654"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractGrokOTP(tt.input); got != tt.want {
				t.Errorf("ExtractGrokOTP(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractDuckMailCode(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"dashed", "Your code: 123-456", "123456"},
		{"plain", "code 482916", "482916"},
		{"first hit wins", "111-222 or 333444", "111222"},
		{"no code", "nothing here", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractDuckMailCode(tt.input); got != tt.want {
				t.Errorf("ExtractDuckMailCode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Pure logic: email matching
// ---------------------------------------------------------------------------

func TestGrokEmailMatchesOTP(t *testing.T) {
	target := "user+gk12345abcd@gmail.com"
	tests := []struct {
		name       string
		msg        GrokEmailMessage
		tag        string
		targetAddr string
		want       bool
	}{
		{
			name: "to matches, from x.ai",
			msg:  GrokEmailMessage{To: target, From: "xAI <noreply@x.ai>", Subject: "Verify"},
			tag:  "gk12345abcd", targetAddr: target, want: true,
		},
		{
			name: "delivered-to matches, from grok",
			msg:  GrokEmailMessage{DeliveredTo: target, From: "Grok <team@grok.com>", Subject: "Hi"},
			tag:  "gk12345abcd", targetAddr: target, want: true,
		},
		{
			name: "tag in to only, subject has code",
			msg:  GrokEmailMessage{To: target, From: "noreply@example.com", Subject: "Your code"},
			tag:  "gk12345abcd", targetAddr: target, want: true,
		},
		{
			name: "subject verif",
			msg:  GrokEmailMessage{To: target, From: "noreply@example.com", Subject: "Email verification"},
			tag:  "gk12345abcd", targetAddr: target, want: true,
		},
		{
			name: "wrong recipient",
			msg:  GrokEmailMessage{To: "someoneelse@gmail.com", From: "xAI <noreply@x.ai>", Subject: "code"},
			tag:  "gk12345abcd", targetAddr: target, want: false,
		},
		{
			name: "right recipient wrong sender/subject",
			msg:  GrokEmailMessage{To: target, From: "spam@evil.com", Subject: "hello"},
			tag:  "gk12345abcd", targetAddr: target, want: false,
		},
		{
			name: "empty target",
			msg:  GrokEmailMessage{To: target, From: "xAI <a@x.ai>", Subject: "code"},
			tag:  "gk12345abcd", targetAddr: "", want: false,
		},
		{
			name: "case insensitive",
			msg:  GrokEmailMessage{To: strings.ToUpper(target), From: "XAI <A@X.AI>", Subject: "CODE"},
			tag:  "GK12345ABCD", targetAddr: target, want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GrokEmailMatchesOTP(tt.msg, tt.targetAddr, tt.tag); got != tt.want {
				t.Errorf("GrokEmailMatchesOTP() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Pure logic: email parsing
// ---------------------------------------------------------------------------

func TestParseGrokEmailMessageSinglePart(t *testing.T) {
	raw := "From: xAI <noreply@x.ai>\r\n" +
		"To: user+gk12345abcd@gmail.com\r\n" +
		"Subject: Your verification code\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Enter 482-916 to verify your account.\r\n"
	msg, err := ParseGrokEmailMessage([]byte(raw))
	if err != nil {
		t.Fatalf("ParseGrokEmailMessage() error = %v", err)
	}
	if msg.From != "xAI <noreply@x.ai>" {
		t.Errorf("From = %q", msg.From)
	}
	if msg.To != "user+gk12345abcd@gmail.com" {
		t.Errorf("To = %q", msg.To)
	}
	if !strings.Contains(msg.Body, "482-916") {
		t.Errorf("Body missing code: %q", msg.Body)
	}
}

func TestParseGrokEmailMessageMultipart(t *testing.T) {
	// "Enter 714-832" base64-encoded.
	raw := "From: Grok <team@grok.com>\r\n" +
		"To: user+gk99999zzzz@gmail.com\r\n" +
		"Delivered-To: user+gk99999zzzz@gmail.com\r\n" +
		"Subject: code\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"BOUNDARY42\"\r\n" +
		"\r\n" +
		"--BOUNDARY42\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"RW50ZXIgNzE0LTgzMg==\r\n" +
		"--BOUNDARY42\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>Enter <b>714-832</b></p>\r\n" +
		"--BOUNDARY42--\r\n"
	msg, err := ParseGrokEmailMessage([]byte(raw))
	if err != nil {
		t.Fatalf("ParseGrokEmailMessage() error = %v", err)
	}
	if msg.DeliveredTo != "user+gk99999zzzz@gmail.com" {
		t.Errorf("DeliveredTo = %q", msg.DeliveredTo)
	}
	if !strings.Contains(msg.Body, "Enter 714-832") {
		t.Errorf("Body missing decoded base64 part: %q", msg.Body)
	}
	if !strings.Contains(msg.Body, "<b>714-832</b>") {
		t.Errorf("Body missing html part: %q", msg.Body)
	}
	if got := ExtractGrokOTP(msg.Subject + " " + msg.Body); got != "714832" {
		t.Errorf("ExtractGrokOTP() = %q, want 714832", got)
	}
}

func TestParseGrokEmailMessageInvalid(t *testing.T) {
	if _, err := ParseGrokEmailMessage([]byte("not an email at all")); err == nil {
		t.Error("expected error for garbage input")
	}
}

// ---------------------------------------------------------------------------
// Pure logic: credentials parsing
// ---------------------------------------------------------------------------

func TestParseGmailCredentialsTOML(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantUser string
		wantPass string
		wantOK   bool
	}{
		{
			name:     "double quotes",
			text:     "[imap]\ngmail_user = \"someone@gmail.com\"\ngmail_app_password = \"abcd efgh ijkl mnop\"\n",
			wantUser: "someone@gmail.com", wantPass: "abcdefghijklmnop", wantOK: true,
		},
		{
			name:     "single quotes no spaces",
			text:     "gmail_user='a@b.c'\ngmail_app_password='xxxx'",
			wantUser: "a@b.c", wantPass: "xxxx", wantOK: true,
		},
		{"missing password", "gmail_user = \"a@b.c\"\n", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, p, ok := ParseGmailCredentialsTOML(tt.text)
			if ok != tt.wantOK || u != tt.wantUser || p != tt.wantPass {
				t.Errorf("got (%q,%q,%v), want (%q,%q,%v)", u, p, ok, tt.wantUser, tt.wantPass, tt.wantOK)
			}
		})
	}
}

func TestParseGmailCredentialsJSON(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wantUser string
		wantPass string
		wantOK   bool
	}{
		{
			name: "lowercase keys", json: `{"gmail_user":"u@gmail.com","gmail_app_password":"a b c d"}`,
			wantUser: "u@gmail.com", wantPass: "abcd", wantOK: true,
		},
		{
			name: "uppercase keys", json: `{"GMAIL_USER":"u@gmail.com","GMAIL_APP_PASSWORD":"zzzz"}`,
			wantUser: "u@gmail.com", wantPass: "zzzz", wantOK: true,
		},
		{"missing", `{"foo":1}`, "", "", false},
		{"invalid json", `{oops`, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, p, ok := ParseGmailCredentialsJSON([]byte(tt.json))
			if ok != tt.wantOK || u != tt.wantUser || p != tt.wantPass {
				t.Errorf("got (%q,%q,%v), want (%q,%q,%v)", u, p, ok, tt.wantUser, tt.wantPass, tt.wantOK)
			}
		})
	}
}

func TestLoadGmailCredentialsEnv(t *testing.T) {
	t.Setenv("GROK_GMAIL_USER", "  env@gmail.com ")
	t.Setenv("GROK_GMAIL_APP_PASSWORD", "aa bb cc")
	u, p := LoadGmailCredentials()
	if u != "env@gmail.com" || p != "aabbcc" {
		t.Errorf("got (%q,%q)", u, p)
	}
}

// ---------------------------------------------------------------------------
// Pure logic: random identity helpers
// ---------------------------------------------------------------------------

func seededRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, 0)) }

func TestGrokGmailAliasTag(t *testing.T) {
	r1, r2 := seededRand(42), seededRand(42)
	a, b := GrokGmailAliasTag(1729000000, r1), GrokGmailAliasTag(1729000000, r2)
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	// 1729000000 % 100000 = 0 -> "gk0" + 4 chars.
	if !strings.HasPrefix(a, "gk0") || len(a) != len("gk0")+4 {
		t.Errorf("unexpected tag format %q", a)
	}
	for _, ch := range a[3:] {
		if !strings.ContainsRune(grokCharsetLowerDigits, ch) {
			t.Errorf("tag suffix char %q not in charset", ch)
		}
	}
}

func TestGrokRandomPassword(t *testing.T) {
	p := GrokRandomPassword(seededRand(7))
	if !strings.HasPrefix(p, "GrokFarm") || !strings.HasSuffix(p, "!@") || len(p) != 8+8+2 {
		t.Errorf("bad password format %q", p)
	}
}

func TestGrokGenerateProfileNames(t *testing.T) {
	first, last := GrokGenerateProfileNames(seededRand(9))
	if !strings.HasPrefix(first, "Grok") || len(first) != 4+1+4 {
		t.Errorf("bad first name %q", first)
	}
	if len(last) != 1+5 {
		t.Errorf("bad last name %q", last)
	}
	isUpper := func(s string) bool { return s >= "A" && s <= "Z" }
	isLower := func(s string) bool { return s >= "a" && s <= "z" }
	if !isUpper(first[4:5]) {
		t.Errorf("first[4] not upper in %q", first)
	}
	for _, ch := range first[5:] {
		if !isLower(string(ch)) {
			t.Errorf("first tail not lower in %q", first)
		}
	}
	if !isUpper(last[0:1]) {
		t.Errorf("last[0] not upper in %q", last)
	}
	for _, ch := range last[1:] {
		if !isLower(string(ch)) {
			t.Errorf("last tail not lower in %q", last)
		}
	}
}

func TestGrokRandomDuckUsername(t *testing.T) {
	u := GrokRandomDuckUsername(seededRand(11))
	if !strings.HasPrefix(u, "grok_") || len(u) != 5+9 {
		t.Errorf("bad duck username %q", u)
	}
}

// ---------------------------------------------------------------------------
// FakeEmailClient
// ---------------------------------------------------------------------------

func TestFakeEmailClient(t *testing.T) {
	ctx := context.Background()
	f := NewFakeEmailClient()
	f.AddMessage("1", GrokEmailMessage{To: "a@b.c", Subject: "s", Body: "b"})
	f.AddMessage("2", GrokEmailMessage{To: "d@e.f", Subject: "s2", Body: "b2"})

	ids, err := f.Search(ctx, "ALL")
	if err != nil || len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("Search() = %v, %v", ids, err)
	}
	m, err := f.Fetch(ctx, "2")
	if err != nil || m.To != "d@e.f" {
		t.Fatalf("Fetch() = %+v, %v", m, err)
	}
	if err := f.MarkRead(ctx, "2"); err != nil {
		t.Fatalf("MarkRead() error = %v", err)
	}
	if got := f.MarkedReadIDs(); len(got) != 1 || got[0] != "2" {
		t.Errorf("MarkedReadIDs() = %v", got)
	}
	if _, err := f.Fetch(ctx, "nope"); err == nil {
		t.Error("expected error for unknown id")
	}
	if err := f.Close(); err != nil || !f.Closed() {
		t.Error("Close() did not mark closed")
	}

	var _ EmailClient = f // compile-time interface check
}

// ---------------------------------------------------------------------------
// GmailMailboxService
// ---------------------------------------------------------------------------

func TestGmailCreateMailbox(t *testing.T) {
	s := NewGmailMailboxService("farmer@gmail.com", "apppass", seededRand(123))
	email, password := s.CreateMailbox()
	if !strings.HasPrefix(email, "farmer+gk") || !strings.HasSuffix(email, "@gmail.com") {
		t.Errorf("bad alias %q", email)
	}
	if s.Tag() == "" || !strings.Contains(email, s.Tag()) {
		t.Errorf("tag %q not in email %q", s.Tag(), email)
	}
	if !strings.HasPrefix(password, "GrokFarm") || !strings.HasSuffix(password, "!@") {
		t.Errorf("bad password %q", password)
	}
}

func TestGmailCreateMailboxStripsDomain(t *testing.T) {
	s := NewGmailMailboxService("farmer", "p", seededRand(1))
	email, _ := s.CreateMailbox()
	if !strings.HasPrefix(email, "farmer+gk") {
		t.Errorf("bad alias %q", email)
	}
}

func TestGmailPollVerificationCode(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeEmailClient()
	// Decoy: right sender, wrong recipient.
	fake.AddMessage("1", GrokEmailMessage{To: "other@gmail.com", From: "xAI <noreply@x.ai>", Subject: "code", Body: "111111"})
	// Target message.
	fake.AddMessage("2", GrokEmailMessage{
		To: "farmer+gk4242wxyz@gmail.com", From: "xAI <noreply@x.ai>",
		Subject: "Your verification code", Body: "Enter 482-916 to continue.",
	})

	s := NewGmailMailboxService("farmer@gmail.com", "pass", seededRand(5))
	s.Email = "farmer+gk4242wxyz@gmail.com"
	s.NewEmailClient = func(ctx context.Context) (EmailClient, error) { return fake, nil }
	s.PollInterval = time.Millisecond

	code, err := s.PollVerificationCode(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("PollVerificationCode() error = %v", err)
	}
	if code != "482916" {
		t.Errorf("code = %q, want 482916", code)
	}
	if got := fake.MarkedReadIDs(); len(got) != 1 || got[0] != "2" {
		t.Errorf("marked read = %v, want [2]", got)
	}
	if !fake.Closed() {
		t.Error("client was not closed after poll round")
	}
}

func TestGmailPollVerificationCodeTimeout(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeEmailClient() // empty inbox
	s := NewGmailMailboxService("farmer@gmail.com", "pass", seededRand(5))
	s.Email = "farmer+gk0000aaaa@gmail.com"
	s.NewEmailClient = func(ctx context.Context) (EmailClient, error) { return fake, nil }
	s.PollInterval = time.Millisecond

	start := time.Now()
	code, err := s.PollVerificationCode(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if code != "" {
		t.Errorf("code = %q, want empty on timeout", code)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("poll did not respect timeout")
	}
}

func TestGmailPollVerificationCodeNoClient(t *testing.T) {
	s := NewGmailMailboxService("farmer@gmail.com", "pass", seededRand(5))
	s.Email = "farmer+gk0000aaaa@gmail.com"
	if _, err := s.PollVerificationCode(context.Background(), time.Second); err == nil {
		t.Error("expected errGrokNoIMAPClient when NewEmailClient is nil")
	}
}

func TestGmailPollVerificationCodeMissingConfig(t *testing.T) {
	s := NewGmailMailboxService("", "", seededRand(5))
	if code, err := s.PollVerificationCode(context.Background(), time.Second); err != nil || code != "" {
		t.Errorf("got (%q,%v), want (\"\",nil)", code, err)
	}
}

// ---------------------------------------------------------------------------
// DuckMailService (httptest)
// ---------------------------------------------------------------------------

func newDuckMailTestServer(t *testing.T, failDomains bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/domains", func(w http.ResponseWriter, r *http.Request) {
		if failDomains {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		// One verified, one with no keys at all (usable by the Python
		// d.get(..., True) default), one explicitly inactive.
		fmt.Fprint(w, `{"hydra:member":[
			{"domain":"mail.tm","isVerified":true,"isActive":true},
			{"domain":"defaultok.example"},
			{"domain":"dead.example","isVerified":false,"isActive":false}
		]}`)
	})
	mux.HandleFunc("/accounts", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"acc-123"}`)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"tok-abc"}`)
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"hydra:member":[{"id":"m1"}]}`)
	})
	mux.HandleFunc("/messages/m1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"text":"Your code is 482916","intro":"","subject":"Verify"}`)
	})
	return httptest.NewServer(mux)
}

func TestDuckMailCreateMailbox(t *testing.T) {
	ctx := context.Background()
	srv := newDuckMailTestServer(t, false)
	defer srv.Close()

	s := NewDuckMailService(srv.Client())
	s.APIBases = []string{srv.URL}
	s.Rand = seededRand(99)

	email, password, err := s.CreateMailbox(ctx)
	if err != nil {
		t.Fatalf("CreateMailbox() error = %v", err)
	}
	if !strings.HasPrefix(email, "grok_") {
		t.Errorf("email = %q", email)
	}
	if s.Token != "tok-abc" || s.AccountID != "acc-123" || s.APIBase != srv.URL {
		t.Errorf("state = %+v", s)
	}
	if !strings.HasPrefix(password, "GrokFarm") {
		t.Errorf("password = %q", password)
	}
}

func TestDuckMailCreateMailboxFallsBackToSecondBase(t *testing.T) {
	ctx := context.Background()
	bad := newDuckMailTestServer(t, true)
	defer bad.Close()
	good := newDuckMailTestServer(t, false)
	defer good.Close()

	s := NewDuckMailService(good.Client())
	s.APIBases = []string{bad.URL, good.URL}
	s.Rand = seededRand(99)

	if _, _, err := s.CreateMailbox(ctx); err != nil {
		t.Fatalf("CreateMailbox() error = %v", err)
	}
	if s.APIBase != good.URL {
		t.Errorf("APIBase = %q, want %q", s.APIBase, good.URL)
	}
}

func TestDuckMailCreateMailboxAllBasesFail(t *testing.T) {
	ctx := context.Background()
	bad := newDuckMailTestServer(t, true)
	defer bad.Close()

	s := NewDuckMailService(bad.Client())
	s.APIBases = []string{bad.URL}
	if _, _, err := s.CreateMailbox(ctx); err == nil {
		t.Error("expected error when all bases fail")
	}
}

func TestDuckMailPollVerificationCode(t *testing.T) {
	ctx := context.Background()
	srv := newDuckMailTestServer(t, false)
	defer srv.Close()

	s := NewDuckMailService(srv.Client())
	s.APIBases = []string{srv.URL}
	s.Rand = seededRand(99)
	if _, _, err := s.CreateMailbox(ctx); err != nil {
		t.Fatalf("CreateMailbox() error = %v", err)
	}
	s.PollInterval = time.Millisecond

	code, err := s.PollVerificationCode(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("PollVerificationCode() error = %v", err)
	}
	if code != "482916" {
		t.Errorf("code = %q, want 482916", code)
	}
}

func TestDuckMailPollWithoutToken(t *testing.T) {
	s := NewDuckMailService(nil)
	if _, err := s.PollVerificationCode(context.Background(), time.Second); err == nil {
		t.Error("expected error when token is missing")
	}
}

// ---------------------------------------------------------------------------
// Proxy helpers
// ---------------------------------------------------------------------------

func TestFindResidentialProxies(t *testing.T) {
	base := t.TempDir()
	out := filepath.Join(base, "output")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# comment\n\nhttp://user:pass@1.2.3.4:8080\n  \nhttp://5.6.7.8:3128\n"
	if err := os.WriteFile(filepath.Join(out, "live_elite.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := FindResidentialProxies(base)
	if len(got) != 2 || got[0] != "http://user:pass@1.2.3.4:8080" || got[1] != "http://5.6.7.8:3128" {
		t.Errorf("got %v", got)
	}
}

func TestFindResidentialProxiesEnvOverride(t *testing.T) {
	base := t.TempDir()
	custom := filepath.Join(base, "custom.txt")
	if err := os.WriteFile(custom, []byte("http://9.9.9.9:9999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXIES_TXT_PATH", custom)
	got := FindResidentialProxies(base)
	if len(got) != 1 || got[0] != "http://9.9.9.9:9999" {
		t.Errorf("got %v", got)
	}
}

func TestFindResidentialProxiesNone(t *testing.T) {
	if got := FindResidentialProxies(t.TempDir()); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestGrokCheckProxyGateway(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	if !GrokCheckProxyGateway("http://" + addr) {
		t.Errorf("expected true for listening %s", addr)
	}
	if !GrokCheckProxyGateway(addr) { // no scheme, like Python's replace chain
		t.Errorf("expected true without scheme for %s", addr)
	}
	if GrokCheckProxyGateway("http://127.0.0.1:1") {
		t.Error("expected false for closed port")
	}
	if GrokCheckProxyGateway("not-a-proxy") {
		t.Error("expected false for garbage")
	}
	if GrokCheckProxyGateway("") {
		t.Error("expected false for empty")
	}
}

func TestGrokFindTurnstileExtension(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "turnstilePatch")
	if err := os.Mkdir(ext, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := GrokFindTurnstileExtension([]string{filepath.Join(dir, "nope"), ext}); got != ext {
		t.Errorf("got %q", got)
	}
	if got := GrokFindTurnstileExtension([]string{filepath.Join(dir, "nope")}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// Browser form helpers with a fake driver
// ---------------------------------------------------------------------------

type fakeGrokBrowser struct {
	calls     []fakeJSCall
	navigated string
	cookies   []GrokCookie
	closed    bool
}

type fakeJSCall struct {
	script string
	args   []any
}

func (f *fakeGrokBrowser) Navigate(_ context.Context, url string) error {
	f.navigated = url
	return nil
}

func (f *fakeGrokBrowser) RunJS(_ context.Context, script string, args ...any) (any, error) {
	f.calls = append(f.calls, fakeJSCall{script: script, args: args})
	switch {
	case strings.Contains(script, "使用邮箱注册"):
		return "Sign up with email", nil
	case strings.Contains(script, `input[autocomplete="email"]`):
		return "clicked", nil
	case strings.Contains(script, "data-input-otp"):
		return "aggregate-filled", nil
	case strings.Contains(script, "t.includes('verify')"):
		return nil, nil
	case strings.Contains(script, "givenName"):
		return true, nil
	default:
		return false, nil
	}
}

func (f *fakeGrokBrowser) Cookies(_ context.Context) ([]GrokCookie, error) {
	return f.cookies, nil
}

func (f *fakeGrokBrowser) Close() error {
	f.closed = true
	return nil
}

func (f *fakeGrokBrowser) sawScript(marker string) bool {
	for _, c := range f.calls {
		if strings.Contains(c.script, marker) {
			return true
		}
	}
	return false
}

var _ GrokBrowserDriver = (*fakeGrokBrowser)(nil)

// fakeGrokDriverNever never succeeds at any script (drives retry timeouts).
type fakeGrokDriverNever struct{ fakeGrokBrowser }

func (f *fakeGrokDriverNever) RunJS(_ context.Context, _ string, _ ...any) (any, error) {
	return false, nil
}

func TestGrokClickEmailSignupButton(t *testing.T) {
	ctx := context.Background()
	if !GrokClickEmailSignupButton(ctx, &fakeGrokBrowser{}, 200*time.Millisecond) {
		t.Error("expected true")
	}
	if GrokClickEmailSignupButton(ctx, &fakeGrokDriverNever{}, 20*time.Millisecond) {
		t.Error("expected false on timeout")
	}
}

func TestGrokFillEmailAndSubmit(t *testing.T) {
	ctx := context.Background()
	if !GrokFillEmailAndSubmit(ctx, &fakeGrokBrowser{}, "a@b.c", 200*time.Millisecond) {
		t.Error("expected true for 'clicked'")
	}
	if GrokFillEmailAndSubmit(ctx, &fakeGrokDriverNever{}, "a@b.c", 20*time.Millisecond) {
		t.Error("expected false on timeout")
	}
}

func TestGrokFillEmailAndSubmitEnter(t *testing.T) {
	ctx := context.Background()
	enter := &fakeGrokDriverEnter{}
	if !GrokFillEmailAndSubmit(ctx, enter, "a@b.c", 200*time.Millisecond) {
		t.Error("expected true for 'enter'")
	}
}

// fakeGrokDriverEnter reports 'enter' for the email script.
type fakeGrokDriverEnter struct{ fakeGrokBrowser }

func (f *fakeGrokDriverEnter) RunJS(_ context.Context, script string, args ...any) (any, error) {
	if strings.Contains(script, `input[autocomplete="email"]`) {
		return "enter", nil
	}
	return false, nil
}

func TestGrokFillOTPAndSubmit(t *testing.T) {
	ctx := context.Background()
	f := &fakeGrokBrowser{}
	if !GrokFillOTPAndSubmit(ctx, f, "482916", 200*time.Millisecond) {
		t.Fatal("expected true")
	}
	if !f.sawScript("t.includes('verify')") {
		t.Error("verify-button submit script was not run after fill")
	}
	found := false
	for _, c := range f.calls {
		if strings.Contains(c.script, "data-input-otp") && len(c.args) == 1 && c.args[0] == "482916" {
			found = true
		}
	}
	if !found {
		t.Error("OTP code was not passed to the fill script")
	}
	if GrokFillOTPAndSubmit(ctx, &fakeGrokDriverNever{}, "482916", 20*time.Millisecond) {
		t.Error("expected false on timeout")
	}
}

func TestGrokFillProfileAndSubmit(t *testing.T) {
	ctx := context.Background()
	f := &fakeGrokBrowser{}
	if !GrokFillProfileAndSubmit(ctx, f, "GrokFarmSecret1!@", seededRand(3), 200*time.Millisecond) {
		t.Fatal("expected true")
	}
	found := false
	for _, c := range f.calls {
		if strings.Contains(c.script, "givenName") && len(c.args) == 3 && c.args[2] == "GrokFarmSecret1!@" {
			found = true
			first, _ := c.args[0].(string)
			if !strings.HasPrefix(first, "Grok") {
				t.Errorf("first name %q missing Grok prefix", first)
			}
		}
	}
	if !found {
		t.Error("profile script not called with (first, last, password)")
	}
}

// ---------------------------------------------------------------------------
// RegisterSingleGrokAccount orchestration (fake browser + fake mail)
// ---------------------------------------------------------------------------

func grokTestProxyFile(t *testing.T, lines ...string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "proxies.txt")
	content := ""
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXIES_TXT_PATH", p)
}

// duckMailTestClient rewrites api.duckmail.sbs / api.mail.tm to the httptest
// server, letting RegisterSingleGrokAccount's internally-built DuckMailService
// talk to the fake.
func duckMailTestClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	rt := srv.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.duckmail.sbs" || r.URL.Host == "api.mail.tm" {
			r.URL.Scheme = "http"
			r.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		}
		return rt.RoundTrip(r)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRegisterSingleGrokAccountDuckMail(t *testing.T) {
	ctx := context.Background()
	srv := newDuckMailTestServer(t, false)
	defer srv.Close()
	grokTestProxyFile(t, "http://user:pw@10.0.0.1:8080")

	browser := &fakeGrokBrowser{cookies: []GrokCookie{{Name: "other", Value: "x"}, {Name: "sso", Value: "sso-token-abc"}}}
	var capturedOpts GrokBrowserOptions
	cfg := GrokRegisterConfig{
		Index: 1, Total: 1, Headless: true,
		MailProvider: "duckmail",
		HTTPClient:   duckMailTestClient(t, srv),
		NewBrowser: func(ctx context.Context, opts GrokBrowserOptions) (GrokBrowserDriver, error) {
			capturedOpts = opts
			return browser, nil
		},
		Rng:    seededRand(2024),
		State:  NewGrokFarmState(),
		Delays: noDelays,
	}

	acc, err := RegisterSingleGrokAccount(ctx, cfg)
	if err != nil {
		t.Fatalf("RegisterSingleGrokAccount() error = %v", err)
	}
	if acc == nil {
		t.Fatal("expected an account, got nil")
	}
	if !strings.HasPrefix(acc.Email, "grok_") {
		t.Errorf("email = %q", acc.Email)
	}
	if acc.SSOToken != "sso-token-abc" {
		t.Errorf("sso = %q", acc.SSOToken)
	}
	if browser.navigated != GrokSignupURL {
		t.Errorf("navigated = %q", browser.navigated)
	}
	if !browser.closed {
		t.Error("browser was not closed")
	}
	if capturedOpts.Proxy != "http://user:pw@10.0.0.1:8080" {
		t.Errorf("proxy opt = %q", capturedOpts.Proxy)
	}
	foundProxyArg := false
	for _, a := range capturedOpts.ExtraArgs {
		if a == "--proxy-server=http://user:pw@10.0.0.1:8080" {
			foundProxyArg = true
		}
	}
	if !foundProxyArg {
		t.Errorf("missing --proxy-server arg in %v", capturedOpts.ExtraArgs)
	}
	if !strings.Contains(acc.CreatedAt, "-") {
		t.Errorf("bad CreatedAt %q", acc.CreatedAt)
	}
}

// gmailHookClient serves one OTP message addressed to a fixed alias. The test
// stubs grokNowUnix and the RNG seed so the alias Register mints is known.
type gmailHookClient struct{ to string }

func (g *gmailHookClient) Search(_ context.Context, _ string) ([]string, error) {
	return []string{"otp1"}, nil
}

func (g *gmailHookClient) Fetch(_ context.Context, _ string) (*GrokEmailMessage, error) {
	return &GrokEmailMessage{
		To:      g.to,
		From:    "xAI <noreply@x.ai>",
		Subject: "Your verification code",
		Body:    "Code 654-321",
	}, nil
}

func (g *gmailHookClient) MarkRead(_ context.Context, _ string) error { return nil }
func (g *gmailHookClient) Close() error                               { return nil }

var _ EmailClient = (*gmailHookClient)(nil)

func TestRegisterSingleGrokAccountGmail(t *testing.T) {
	ctx := context.Background()
	grokTestProxyFile(t) // empty file -> no proxies chosen

	// Fix the clock + RNG so the minted alias is predictable.
	oldNow := grokNowUnix
	grokNowUnix = func() int64 { return 1700000000 } // %100000 == 0
	defer func() { grokNowUnix = oldNow }()
	expectedTag := GrokGmailAliasTag(1700000000, seededRand(77))
	expectedEmail := "farmer+" + expectedTag + "@gmail.com"

	browser := &fakeGrokBrowser{cookies: []GrokCookie{{Name: "sso-rw", Value: "rw-token"}}}
	var capturedOpts GrokBrowserOptions
	cfg := GrokRegisterConfig{
		Index: 2, Total: 3, Headless: false,
		MailProvider:     "gmail",
		GmailUser:        "farmer@gmail.com",
		GmailAppPassword: "apppass",
		NewEmailClient: func(ctx context.Context) (EmailClient, error) {
			return &gmailHookClient{to: expectedEmail}, nil
		},
		NewBrowser: func(ctx context.Context, opts GrokBrowserOptions) (GrokBrowserDriver, error) {
			capturedOpts = opts
			return browser, nil
		},
		Rng:    seededRand(77),
		State:  NewGrokFarmState(),
		Delays: noDelays,
	}

	acc, err := RegisterSingleGrokAccount(ctx, cfg)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if acc == nil {
		t.Fatal("expected account")
	}
	if acc.Email != expectedEmail {
		t.Errorf("email = %q, want %q", acc.Email, expectedEmail)
	}
	if acc.SSOToken != "rw-token" {
		t.Errorf("sso = %q (want first-match sso-rw)", acc.SSOToken)
	}
	otpTyped := false
	for _, c := range browser.calls {
		if strings.Contains(c.script, "data-input-otp") && len(c.args) == 1 && c.args[0] == "654321" {
			otpTyped = true
		}
	}
	if !otpTyped {
		t.Error("OTP 654321 was not typed into the form")
	}
	if capturedOpts.Proxy != "" {
		t.Errorf("expected no proxy (empty proxy file), got %q", capturedOpts.Proxy)
	}
	if capturedOpts.WindowSize != "" {
		t.Errorf("non-headless should not set window size, got %q", capturedOpts.WindowSize)
	}
}

func TestRegisterSingleGrokAccountNoBrowserFactory(t *testing.T) {
	_, err := RegisterSingleGrokAccount(context.Background(), GrokRegisterConfig{Index: 1, Total: 1})
	if err == nil {
		t.Error("expected error when NewBrowser is nil")
	}
}

func TestRegisterSingleGrokAccountOTPFail(t *testing.T) {
	grokTestProxyFile(t) // no proxies
	empty := NewFakeEmailClient()
	cfg := GrokRegisterConfig{
		Index: 1, Total: 1,
		MailProvider:     "gmail",
		GmailUser:        "farmer@gmail.com",
		GmailAppPassword: "p",
		NewEmailClient:   func(ctx context.Context) (EmailClient, error) { return empty, nil },
		NewBrowser: func(ctx context.Context, opts GrokBrowserOptions) (GrokBrowserDriver, error) {
			return &fakeGrokBrowser{}, nil
		},
		Rng:    seededRand(1),
		State:  NewGrokFarmState(),
		Delays: noDelays,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	acc, err := RegisterSingleGrokAccount(ctx, cfg)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if acc != nil {
		t.Error("expected nil account when OTP never arrives")
	}
}

// ---------------------------------------------------------------------------
// SaveGrokAccount
// ---------------------------------------------------------------------------

func TestSaveGrokAccount(t *testing.T) {
	dir := t.TempDir()
	a1 := GrokAccount{Email: "a@x.ai", Password: "pw1", SSOToken: "tok1", CreatedAt: "2026-10-17 21:00:00"}
	a2 := GrokAccount{Email: "b@x.ai", Password: "pw2", CreatedAt: "2026-10-17 21:01:00"}

	if err := SaveGrokAccount(a1, dir); err != nil {
		t.Fatal(err)
	}
	if err := SaveGrokAccount(a2, dir); err != nil {
		t.Fatal(err)
	}

	txt, err := os.ReadFile(filepath.Join(dir, "grok_accounts.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(txt)), "\n")
	if len(lines) != 2 {
		t.Fatalf("txt lines = %v", lines)
	}
	if lines[0] != "a@x.ai----pw1----tok1" {
		t.Errorf("line0 = %q", lines[0])
	}
	if lines[1] != "b@x.ai----pw2----NO_SSO_COOKIE" {
		t.Errorf("line1 = %q", lines[1])
	}

	raw, err := os.ReadFile(filepath.Join(dir, "grok_accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var arr []GrokAccount
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 2 || arr[0].Email != "a@x.ai" || arr[1].SSOToken != "" {
		t.Errorf("json = %+v", arr)
	}
}

// ---------------------------------------------------------------------------
// RunGrokFarm
// ---------------------------------------------------------------------------

func TestRunGrokFarm(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := NewGrokFarmState()

	calls := 0
	cfg := GrokFarmConfig{
		Total:           3,
		Headless:        true,
		MailProvider:    "gmail",
		OutputDir:       dir,
		State:           st,
		BetweenAccounts: time.Millisecond,
		Register: func(ctx context.Context, rcfg GrokRegisterConfig) (*GrokAccount, error) {
			calls++
			if rcfg.Index != calls || rcfg.Total != 3 {
				t.Errorf("register got index=%d total=%d", rcfg.Index, rcfg.Total)
			}
			if rcfg.State == nil {
				t.Error("register did not receive the shared state")
			}
			if calls == 2 {
				return nil, nil // one graceful failure
			}
			return &GrokAccount{
				Email:     fmt.Sprintf("acc%d@x.ai", calls),
				Password:  "pw",
				SSOToken:  "tok",
				CreatedAt: "2026-10-17 21:00:00",
			}, nil
		},
	}

	harvested, err := RunGrokFarm(ctx, cfg)
	if err != nil {
		t.Fatalf("RunGrokFarm() error = %v", err)
	}
	if len(harvested) != 2 {
		t.Errorf("harvested = %d, want 2", len(harvested))
	}
	if calls != 3 {
		t.Errorf("register calls = %d, want 3", calls)
	}

	snap := st.Snapshot()
	if snap.Status != GrokStatusCompleted || snap.Progress != 100 || snap.CurrentStep != GrokStepCompleted {
		t.Errorf("final state = %+v", snap)
	}
	if snap.HarvestedCount != 2 || len(snap.HarvestedAccounts) != 2 {
		t.Errorf("harvested in state = %+v", snap.HarvestedAccounts)
	}
	if !snap.HarvestedAccounts[0].HasSSO {
		t.Error("HasSSO should be true")
	}
	if snap.CurrentAccount != 3 || snap.TotalAccounts != 3 {
		t.Errorf("counters = %d/%d", snap.CurrentAccount, snap.TotalAccounts)
	}
	if _, err := os.Stat(filepath.Join(dir, "grok_accounts.txt")); err != nil {
		t.Errorf("accounts txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "grok_accounts.json")); err != nil {
		t.Errorf("accounts json missing: %v", err)
	}
}

func TestRunGrokFarmZeroTotal(t *testing.T) {
	harvested, err := RunGrokFarm(context.Background(), GrokFarmConfig{
		Total:     0,
		State:     NewGrokFarmState(),
		OutputDir: t.TempDir(),
		Register: func(ctx context.Context, c GrokRegisterConfig) (*GrokAccount, error) {
			if c.Total != 1 {
				t.Errorf("total = %d, want 1", c.Total)
			}
			return nil, nil
		},
		BetweenAccounts: time.Millisecond,
	})
	if err != nil || len(harvested) != 0 {
		t.Errorf("got (%d,%v)", len(harvested), err)
	}
}

// ---------------------------------------------------------------------------
// GrokFarmState
// ---------------------------------------------------------------------------

func TestGrokFarmStateLogCap(t *testing.T) {
	st := NewGrokFarmState()
	for i := 0; i < GrokMaxStateLogs+10; i++ {
		st.Log(fmt.Sprintf("msg %d", i), GrokLogInfo, "")
	}
	snap := st.Snapshot()
	if len(snap.Logs) != GrokMaxStateLogs {
		t.Errorf("logs = %d, want cap %d", len(snap.Logs), GrokMaxStateLogs)
	}
	if !strings.Contains(snap.Logs[0], fmt.Sprintf("msg %d", 10)) {
		t.Errorf("oldest log not evicted: %q", snap.Logs[0])
	}
	st.Log("step msg", GrokLogInfo, GrokStepWaitingOTP)
	if got := st.Snapshot().CurrentStep; got != GrokStepWaitingOTP {
		t.Errorf("step = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Browser options
// ---------------------------------------------------------------------------

func TestGrokBuildBrowserOptions(t *testing.T) {
	cfg := GrokRegisterConfig{Headless: true}
	opts := GrokBuildBrowserOptions(cfg, "http://1.2.3.4:8080", "/ext/turnstilePatch")
	if opts.UserAgent != GrokChromeUserAgent {
		t.Errorf("UA = %q", opts.UserAgent)
	}
	if opts.WindowSize != "1920,1080" {
		t.Errorf("WindowSize = %q", opts.WindowSize)
	}
	joined := strings.Join(opts.ExtraArgs, " ")
	for _, want := range []string{
		"--disable-blink-features=AutomationControlled", "--no-sandbox",
		"--disable-dev-shm-usage", "--window-size=1920,1080",
		"--proxy-server=http://1.2.3.4:8080",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing arg %q in %v", want, opts.ExtraArgs)
		}
	}
	if len(opts.Extensions) != 1 || opts.Extensions[0] != "/ext/turnstilePatch" {
		t.Errorf("extensions = %v", opts.Extensions)
	}

	cfg2 := GrokRegisterConfig{Headless: false, UserAgent: "custom"}
	opts2 := GrokBuildBrowserOptions(cfg2, "", "")
	if opts2.UserAgent != "custom" || opts2.WindowSize != "" {
		t.Errorf("non-headless opts = %+v", opts2)
	}
	if len(opts2.Extensions) != 0 {
		t.Errorf("extensions = %v", opts2.Extensions)
	}
}
