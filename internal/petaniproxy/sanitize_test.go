package petaniproxy

import (
	"net/http"
	"testing"
)

func inStringList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestSanitizeHeaders(t *testing.T) {
	newHdr := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(pairs); i += 2 {
			h.Set(pairs[i], pairs[i+1])
		}
		return h
	}
	normalUA := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/120 Safari/537.36"

	tests := []struct {
		name  string
		in    http.Header
		check func(t *testing.T, out http.Header)
	}{
		{
			name: "bot user-agent is rotated",
			in:   newHdr("User-Agent", "python-requests/2.31.0"),
			check: func(t *testing.T, out http.Header) {
				ua := out.Get("User-Agent")
				if ua == "python-requests/2.31.0" {
					t.Error("bot UA was not rotated")
				}
				if !inStringList(CommonUserAgents, ua) {
					t.Errorf("rotated UA %q not in CommonUserAgents", ua)
				}
			},
		},
		{
			name: "curl user-agent is rotated",
			in:   newHdr("User-Agent", "curl/8.4.0"),
			check: func(t *testing.T, out http.Header) {
				if ua := out.Get("User-Agent"); !inStringList(CommonUserAgents, ua) {
					t.Errorf("UA %q not rotated into pool", ua)
				}
			},
		},
		{
			name: "normal user-agent is preserved",
			in:   newHdr("User-Agent", normalUA),
			check: func(t *testing.T, out http.Header) {
				if got := out.Get("User-Agent"); got != normalUA {
					t.Errorf("User-Agent = %q, want preserved %q", got, normalUA)
				}
			},
		},
		{
			name: "missing user-agent is added",
			in:   newHdr("X-Custom", "keep"),
			check: func(t *testing.T, out http.Header) {
				if ua := out.Get("User-Agent"); !inStringList(CommonUserAgents, ua) {
					t.Errorf("UA %q not added from pool", ua)
				}
			},
		},
		{
			name: "blocked deanonymizing headers stripped",
			in: newHdr(
				"User-Agent", normalUA,
				"X-Forwarded-For", "9.9.9.9",
				"Via", "1.1 evil-proxy",
				"X-Real-Ip", "9.9.9.9",
				"Cf-Connecting-Ip", "9.9.9.9",
				"X-Proxy-User-Agent", "spy",
				"X-Custom", "keep",
			),
			check: func(t *testing.T, out http.Header) {
				for _, k := range []string{"X-Forwarded-For", "Via", "X-Real-Ip", "Cf-Connecting-Ip", "X-Proxy-User-Agent"} {
					if v := out.Get(k); v != "" {
						t.Errorf("header %s = %q, want stripped", k, v)
					}
				}
				if got := out.Get("X-Custom"); got != "keep" {
					t.Errorf("X-Custom = %q, want preserved", got)
				}
			},
		},
		{
			name: "rfc hop-by-hop headers stripped",
			in: newHdr(
				"User-Agent", normalUA,
				"Connection", "keep-alive",
				"Keep-Alive", "timeout=5",
				"Transfer-Encoding", "chunked",
				"Te", "trailers",
				"Upgrade", "websocket",
				"Proxy-Connection", "keep-alive",
				"X-Custom", "keep",
			),
			check: func(t *testing.T, out http.Header) {
				for _, k := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Te", "Upgrade", "Proxy-Connection"} {
					if v := out.Get(k); v != "" {
						t.Errorf("hop-by-hop header %s = %q, want stripped", k, v)
					}
				}
				if got := out.Get("X-Custom"); got != "keep" {
					t.Errorf("X-Custom = %q, want preserved", got)
				}
			},
		},
		{
			name: "headers named in Connection token are stripped",
			in: func() http.Header {
				h := newHdr("User-Agent", normalUA, "X-Custom", "keep")
				h.Set("Connection", "close, X-Secret")
				h.Set("X-Secret", "shh")
				return h
			}(),
			check: func(t *testing.T, out http.Header) {
				if v := out.Get("X-Secret"); v != "" {
					t.Errorf("X-Secret = %q, want stripped via Connection token", v)
				}
				if got := out.Get("X-Custom"); got != "keep" {
					t.Errorf("X-Custom = %q, want preserved", got)
				}
			},
		},
	}

	s := NewSanitizer()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, s.Sanitize(tt.in))
		})
	}
}

func TestSanitizeDoesNotMutateInput(t *testing.T) {
	s := NewSanitizer()
	in := http.Header{
		"User-Agent":      {"python-requests/2.31.0"},
		"X-Forwarded-For": {"9.9.9.9"},
	}
	_ = s.Sanitize(in)
	if in.Get("User-Agent") != "python-requests/2.31.0" {
		t.Error("input User-Agent mutated")
	}
	if in.Get("X-Forwarded-For") != "9.9.9.9" {
		t.Error("input X-Forwarded-For mutated")
	}
}

func TestSanitizerCustomPool(t *testing.T) {
	s := NewSanitizer()
	s.UserAgents = []string{"CustomAgent/1.0"}
	out := s.Sanitize(http.Header{})
	if got := out.Get("User-Agent"); got != "CustomAgent/1.0" {
		t.Errorf("User-Agent = %q, want CustomAgent/1.0", got)
	}
}
