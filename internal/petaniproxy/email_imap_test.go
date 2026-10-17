package petaniproxy

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap"
)

// ---------------------------------------------------------------------------
// Criteria parsing (pure, no network)
// ---------------------------------------------------------------------------

func TestIMAPSearchCriteria(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantErr     bool
		wantUnseen  bool
		wantSeen    bool
		wantFrom    string
		wantSubject string
	}{
		{name: "unseen only", input: "UNSEEN", wantUnseen: true},
		{name: "lowercase unseen", input: "unseen", wantUnseen: true},
		{name: "unseen from", input: "UNSEEN FROM noreply@x.ai", wantUnseen: true, wantFrom: "noreply@x.ai"},
		{name: "unseen subject single word", input: "UNSEEN SUBJECT verify", wantUnseen: true, wantSubject: "verify"},
		{name: "unseen subject multi word", input: "UNSEEN SUBJECT your verification code", wantUnseen: true, wantSubject: "your verification code"},
		{name: "unseen from and subject", input: "UNSEEN FROM info@x.ai SUBJECT verification code", wantUnseen: true, wantFrom: "info@x.ai", wantSubject: "verification code"},
		{name: "seen only", input: "SEEN", wantSeen: true},
		{name: "extra whitespace", input: "  UNSEEN   FROM   a@b.c  ", wantUnseen: true, wantFrom: "a@b.c"},
		{name: "empty", input: "", wantErr: true},
		{name: "whitespace only", input: "   ", wantErr: true},
		{name: "unknown token", input: "UNSEEN FROBNICATE x", wantErr: true},
		{name: "from without address", input: "UNSEEN FROM", wantErr: true},
		{name: "subject without text", input: "UNSEEN SUBJECT", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crit, err := parseIMAPSearchCriteria(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseIMAPSearchCriteria(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			hasUnseen := false
			hasSeen := false
			for _, f := range crit.WithoutFlags {
				if f == imap.SeenFlag {
					hasUnseen = true
				}
			}
			for _, f := range crit.WithFlags {
				if f == imap.SeenFlag {
					hasSeen = true
				}
			}
			if hasUnseen != tt.wantUnseen {
				t.Errorf("UNSEEN flag = %v, want %v", hasUnseen, tt.wantUnseen)
			}
			if hasSeen != tt.wantSeen {
				t.Errorf("SEEN flag = %v, want %v", hasSeen, tt.wantSeen)
			}
			if tt.wantFrom != "" {
				if got := crit.Header.Get("From"); got != tt.wantFrom {
					t.Errorf("Header From = %q, want %q", got, tt.wantFrom)
				}
			}
			if tt.wantSubject != "" {
				if got := crit.Header.Get("Subject"); got != tt.wantSubject {
					t.Errorf("Header Subject = %q, want %q", got, tt.wantSubject)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Constructor validation — must fail before any network dial
// ---------------------------------------------------------------------------

func TestIMAPClientValidation(t *testing.T) {
	if _, err := NewIMAPEmailClient("", "user", "pass", true); err == nil {
		t.Error("empty addr: expected error, got nil")
	} else if !strings.Contains(err.Error(), "addr") {
		t.Errorf("empty addr: error %q should mention addr", err)
	}
	if _, err := NewIMAPEmailClient("  ", "user", "pass", true); err == nil {
		t.Error("whitespace addr: expected error, got nil")
	}
	if _, err := NewIMAPEmailClient("imap.gmail.com:993", "", "pass", true); err == nil {
		t.Error("empty username: expected error, got nil")
	}
	if _, err := NewIMAPEmailClient("imap.gmail.com:993", "user", "", true); err == nil {
		t.Error("empty password: expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Raw message parsing (pure, no network)
// ---------------------------------------------------------------------------

const imapSampleMultipart = "From: Grok <noreply@x.ai>\r\n" +
	"To: farmer+tag007@gmail.com\r\n" +
	"Delivered-To: farmer+tag007@gmail.com\r\n" +
	"Subject: Your Grok verification code\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"BOUNDARY42\"\r\n" +
	"\r\n" +
	"--BOUNDARY42\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Your code is 482-916. It expires in 10 minutes.\r\n" +
	"--BOUNDARY42\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<html><body><p>Your code is <b>482-916</b>.</p></body></html>\r\n" +
	"--BOUNDARY42--\r\n"

const imapSampleHTMLOnly = "From: =?UTF-8?Q?X=2Eai_Verify?= <verify@x.ai>\r\n" +
	"To: farmer@gmail.com\r\n" +
	"Subject: Code\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"<div>Enter <span>739201</span> to continue</div>\r\n"

const imapSamplePlainSingle = "From: alerts@example.com\r\n" +
	"To: farmer@gmail.com\r\n" +
	"Subject: plain note\r\n" +
	"\r\n" +
	"hello world 123456\r\n"

func TestIMAPParseRawMultipart(t *testing.T) {
	msg, err := parseGrokEmailRaw([]byte(imapSampleMultipart))
	if err != nil {
		t.Fatalf("parseGrokEmailRaw() error = %v", err)
	}
	if !strings.Contains(msg.From, "noreply@x.ai") {
		t.Errorf("From = %q, want noreply@x.ai", msg.From)
	}
	if !strings.Contains(msg.To, "farmer+tag007@gmail.com") {
		t.Errorf("To = %q, want farmer+tag007@gmail.com", msg.To)
	}
	if !strings.Contains(msg.DeliveredTo, "farmer+tag007@gmail.com") {
		t.Errorf("DeliveredTo = %q, want farmer+tag007@gmail.com", msg.DeliveredTo)
	}
	if msg.Subject != "Your Grok verification code" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	// text/plain preferred over text/html
	if !strings.Contains(msg.Body, "482-916") {
		t.Errorf("Body = %q, want OTP 482-916", msg.Body)
	}
	if strings.Contains(msg.Body, "<b>") {
		t.Errorf("Body = %q, should be plain text, not HTML", msg.Body)
	}
}

func TestIMAPParseRawHTMLFallback(t *testing.T) {
	msg, err := parseGrokEmailRaw([]byte(imapSampleHTMLOnly))
	if err != nil {
		t.Fatalf("parseGrokEmailRaw() error = %v", err)
	}
	// RFC2047 subject/from decoded
	if msg.From != "X.ai Verify <verify@x.ai>" {
		t.Errorf("From = %q, want decoded display name", msg.From)
	}
	if !strings.Contains(msg.Body, "739201") {
		t.Errorf("Body = %q, want OTP 739201", msg.Body)
	}
	if strings.ContainsAny(msg.Body, "<>") {
		t.Errorf("Body = %q, HTML tags should be stripped", msg.Body)
	}
}

func TestIMAPParseRawPlainSinglepart(t *testing.T) {
	msg, err := parseGrokEmailRaw([]byte(imapSamplePlainSingle))
	if err != nil {
		t.Fatalf("parseGrokEmailRaw() error = %v", err)
	}
	if !strings.Contains(msg.Body, "123456") {
		t.Errorf("Body = %q, want 123456", msg.Body)
	}
	if msg.DeliveredTo != "" {
		t.Errorf("DeliveredTo = %q, want empty when header absent", msg.DeliveredTo)
	}
}

func TestIMAPParseRawInvalid(t *testing.T) {
	if _, err := parseGrokEmailRaw([]byte("")); err == nil {
		t.Error("empty input: expected error, got nil")
	}
	if _, err := parseGrokEmailRaw([]byte("   \r\n  ")); err == nil {
		t.Error("whitespace-only input: expected error, got nil")
	}
	// go-message rejects malformed header lines.
	if _, err := parseGrokEmailRaw([]byte("not an email at all")); err == nil {
		t.Error("malformed header: expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// HTML stripping helper
// ---------------------------------------------------------------------------

func TestIMAPStripHTML(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"tags removed", "<p>code <b>123456</b></p>", "code 123456"},
		{"entities", "a&nbsp;b &amp; c", "a b & c"},
		{"multiline", "<div>\n<span>739201</span>\n</div>", "739201"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripIMAPHTML(tt.input); got != tt.want {
				t.Errorf("stripIMAPHTML(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIMAPDecodeHeaderValue(t *testing.T) {
	if got := decodeIMAPHeaderValue("plain subject"); got != "plain subject" {
		t.Errorf("plain = %q", got)
	}
	if got := decodeIMAPHeaderValue("=?UTF-8?Q?Verifikasi_akun_Anda?="); got != "Verifikasi akun Anda" {
		t.Errorf("rfc2047 = %q", got)
	}
	if got := decodeIMAPHeaderValue(""); got != "" {
		t.Errorf("empty = %q", got)
	}
}
