package petaniproxy

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseProxyURL(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantNil    bool
		wantScheme string
		wantHost   string
	}{
		{"empty", "", true, "", ""},
		{"blank", "   ", true, "", ""},
		{"bare hostport gets http", "127.0.0.1:8080", false, "http", "127.0.0.1"},
		{"scheme kept", "socks5h://h:1080", false, "socks5h", "h"},
		{"https kept", "https://h:443", false, "https", "h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := ParseProxyURL(tt.input)
			if err != nil {
				t.Fatalf("ParseProxyURL(%q): %v", tt.input, err)
			}
			if tt.wantNil {
				if u != nil {
					t.Fatalf("ParseProxyURL(%q) = %v, want nil", tt.input, u)
				}
				return
			}
			if u == nil {
				t.Fatalf("ParseProxyURL(%q) = nil", tt.input)
			}
			if u.Scheme != tt.wantScheme || u.Hostname() != tt.wantHost {
				t.Errorf("ParseProxyURL(%q) = %s://%s, want %s://%s",
					tt.input, u.Scheme, u.Hostname(), tt.wantScheme, tt.wantHost)
			}
		})
	}
}

func TestSafeProxyPort(t *testing.T) {
	u, _ := url.Parse("http://h:8080")
	if p, err := SafeProxyPort(u); err != nil || p != 8080 {
		t.Errorf("SafeProxyPort = %d, %v; want 8080, nil", p, err)
	}
	u2, _ := url.Parse("http://h")
	if p, err := SafeProxyPort(u2); err != nil || p != 0 {
		t.Errorf("SafeProxyPort = %d, %v; want 0, nil", p, err)
	}
	if p, err := SafeProxyPort(nil); err != nil || p != 0 {
		t.Errorf("SafeProxyPort(nil) = %d, %v", p, err)
	}
	// url.Parse rejects bad ports, so build the URL struct directly.
	bad := &url.URL{Host: "h:abc"}
	if _, err := SafeProxyPort(bad); err == nil {
		t.Error("SafeProxyPort(h:abc) should error like parsed.port raising")
	}
}

func TestProxyHasAuth(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"userpass", "http://user:pass@h:8080", true},
		{"user only", "http://user@h:8080", true},
		{"empty user with password", "http://:pass@h:8080", true},
		{"no auth", "http://h:8080", false},
		{"bare", "h:8080", false},
		{"empty", "", false},
		{"socks no auth", "socks5://h:1080", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProxyHasAuth(tt.input); got != tt.want {
				t.Errorf("ProxyHasAuth(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNewLocalProxyBridge(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantErr    bool
		wantScheme string
		wantPort   int
		wantAuth   string // expected AuthHeader, "" when none
	}{
		{"http explicit", "http://127.0.0.1:8080", false, "http", 8080, ""},
		{"bare defaults http 80", "127.0.0.1", false, "http", 80, ""},
		{"https defaults 443", "https://h", false, "https", 443, ""},
		{"socks5 defaults 1080", "socks5://h", false, "socks5", 1080, ""},
		{"socks4a kept", "socks4a://h:1080", false, "socks4a", 1080, ""},
		{"socks5h kept", "socks5h://h:1080", false, "socks5h", 1080, ""},
		{"auth header", "http://u:p@h:8080", false, "http", 8080, base64.StdEncoding.EncodeToString([]byte("u:p"))},
		{"empty invalid", "", true, "", 0, ""},
		{"no host invalid", "http://", true, "", 0, ""},
		{"unsupported scheme", "ftp://h:21", true, "", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := NewLocalProxyBridge(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NewLocalProxyBridge(%q) expected error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewLocalProxyBridge(%q): %v", tt.input, err)
			}
			if b.UpstreamScheme != tt.wantScheme || b.UpstreamPort != tt.wantPort {
				t.Errorf("got %s:%d, want %s:%d", b.UpstreamScheme, b.UpstreamPort, tt.wantScheme, tt.wantPort)
			}
			if b.AuthHeader != tt.wantAuth {
				t.Errorf("AuthHeader = %q, want %q", b.AuthHeader, tt.wantAuth)
			}
			if b.Timeout != DefaultBridgeTimeout || b.RelayTimeout != DefaultBridgeRelayTimeout {
				t.Errorf("timeouts = %v/%v, want defaults", b.Timeout, b.RelayTimeout)
			}
		})
	}
}

func TestSplitHostPort(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		def       int
		wantHost  string
		wantPort  int
		wantErr   bool
		wantKind  string // BridgeError kind when wantErr
		wantBrErr bool   // true when the error must be a *BridgeError
	}{
		{"host:port", "example.com:3128", 80, "example.com", 3128, false, "", false},
		{"bare host", "example.com", 80, "example.com", 80, false, "", false},
		{"ipv6 with port", "[::1]:8080", 80, "::1", 8080, false, "", false},
		{"ipv6 no port", "[::1]", 443, "::1", 443, false, "", false},
		{"ipv6 no bracket uses default", "::1", 80, "::1", 80, false, "", false},
		{"bad ipv6", "[::1", 80, "", 0, true, "target", true},
		{"bad port is plain error", "h:abc", 80, "", 0, true, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, err := splitHostPort(tt.input, tt.def)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("splitHostPort(%q) expected error", tt.input)
				}
				var be *BridgeError
				if tt.wantBrErr {
					if !asBridgeError(err, &be) || be.Kind != tt.wantKind {
						t.Errorf("err = %v, want BridgeError kind %q", err, tt.wantKind)
					}
				} else if asBridgeError(err, &be) {
					t.Errorf("err = %v, want plain (non-Bridge) error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitHostPort(%q): %v", tt.input, err)
			}
			if host != tt.wantHost || port != tt.wantPort {
				t.Errorf("got %s:%d, want %s:%d", host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func asBridgeError(err error, target **BridgeError) bool {
	if be, ok := err.(*BridgeError); ok {
		*target = be
		return true
	}
	return false
}

func TestRewriteHTTPRequest(t *testing.T) {
	t.Run("absolute-form to origin-form", func(t *testing.T) {
		in := []byte("GET http://example.com:8080/a?b=1 HTTP/1.1\r\nHost: example.com:8080\r\nProxy-Authorization: Basic eHh4\r\n\r\nbodybytes")
		host, port, data, err := rewriteHTTPRequest(in)
		if err != nil {
			t.Fatalf("rewriteHTTPRequest: %v", err)
		}
		if host != "example.com" || port != 8080 {
			t.Errorf("host:port = %s:%d", host, port)
		}
		s := string(data)
		if !strings.HasPrefix(s, "GET /a?b=1 HTTP/1.1\r\n") {
			t.Errorf("request line not rewritten: %q", s[:40])
		}
		if strings.Contains(strings.ToLower(s), "proxy-authorization") {
			t.Error("Proxy-Authorization not stripped")
		}
		if !strings.HasSuffix(s, "\r\n\r\nbodybytes") {
			t.Error("body not preserved")
		}
	})

	t.Run("https default port", func(t *testing.T) {
		in := []byte("GET https://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
		host, port, _, err := rewriteHTTPRequest(in)
		if err != nil {
			t.Fatalf("rewriteHTTPRequest: %v", err)
		}
		if host != "example.com" || port != 443 {
			t.Errorf("host:port = %s:%d, want example.com:443", host, port)
		}
	})

	t.Run("origin-form uses Host header", func(t *testing.T) {
		in := []byte("GET /a HTTP/1.1\r\nhost: example.com:9000\r\n\r\n")
		host, port, data, err := rewriteHTTPRequest(in)
		if err != nil {
			t.Fatalf("rewriteHTTPRequest: %v", err)
		}
		if host != "example.com" || port != 9000 {
			t.Errorf("host:port = %s:%d", host, port)
		}
		if !strings.HasPrefix(string(data), "GET /a HTTP/1.1\r\n") {
			t.Errorf("origin-form line changed: %q", string(data)[:30])
		}
	})

	t.Run("invalid request line", func(t *testing.T) {
		_, _, _, err := rewriteHTTPRequest([]byte("GARBAGE\r\n\r\n"))
		var be *BridgeError
		if !asBridgeError(err, &be) || be.Kind != "request" {
			t.Errorf("err = %v, want BridgeError kind request", err)
		}
	})
}

func TestInjectProxyAuth(t *testing.T) {
	b, _ := NewLocalProxyBridge("http://u:p@127.0.0.1:8080")
	in := []byte("GET http://h/ HTTP/1.1\r\nHost: h\r\n\r\n")
	got := b.InjectProxyAuth(in)
	if !strings.Contains(string(got), "Proxy-Authorization: Basic "+b.AuthHeader+"\r\n") {
		t.Errorf("auth header not injected: %q", got)
	}
	// Already present -> unchanged.
	if again := b.InjectProxyAuth(got); string(again) != string(got) {
		t.Error("double injection changed the request")
	}
	// Case-insensitive existing header -> unchanged.
	lower := []byte("GET http://h/ HTTP/1.1\r\nHost: h\r\nproxy-authorization: Basic xx\r\n\r\n")
	if out := b.InjectProxyAuth(lower); string(out) != string(lower) {
		t.Error("existing lowercase header not respected")
	}
	// No header terminator -> unchanged.
	noTerm := []byte("GET http://h/ HTTP/1.1\r\nHost: h\r\n")
	if out := b.InjectProxyAuth(noTerm); string(out) != string(noTerm) {
		t.Error("unterminated request changed")
	}
	// Bridge without auth -> unchanged.
	plain, _ := NewLocalProxyBridge("http://127.0.0.1:8080")
	if out := plain.InjectProxyAuth(in); string(out) != string(in) {
		t.Error("no-auth bridge changed the request")
	}
}

func TestProxyForChromium(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"empty", "", "", false},
		{"plain http", "http://127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"bare gets http", "127.0.0.1:3128", "http://127.0.0.1:3128", false},
		{"socks5h normalized", "socks5h://h:1080", "socks5://h:1080", false},
		{"socks5 kept", "socks5://h:1080", "socks5://h:1080", false},
		{"https default port", "https://h", "https://h:443", false},
		{"auth rejected", "http://u:p@h:8080", "", true},
		{"socks4a rejected", "socks4a://h:1080", "", true},
		{"no host", "http://", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ProxyForChromium(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ProxyForChromium(%q) expected error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProxyForChromium(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ProxyForChromium(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBridgeDiagnostics(t *testing.T) {
	b, _ := NewLocalProxyBridge("http://127.0.0.1:8080")
	if b.Diagnostic() != nil {
		t.Error("Diagnostic() should be nil before any failure")
	}
	b.RecordDiagnostic("upstream_connect", "boom")
	d := b.Diagnostic()
	if d == nil || d.Kind != "upstream_connect" || d.Message != "boom" || d.At <= 0 {
		t.Errorf("unexpected diagnostic: %+v", d)
	}
	b.RecordDiagnostic("k", strings.Repeat("x", 600))
	if got := b.Diagnostic(); len(got.Message) != 500 {
		t.Errorf("message not truncated to 500: %d", len(got.Message))
	}
}

// --- live bridge tests (localhost only, no external network) ---

func testBridge(t *testing.T, upstream string) *LocalProxyBridge {
	t.Helper()
	b, err := NewLocalProxyBridge(upstream)
	if err != nil {
		t.Fatalf("NewLocalProxyBridge(%q): %v", upstream, err)
	}
	b.Timeout = 5 * time.Second
	b.RelayTimeout = 5 * time.Second
	return b
}

func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return conn
}

// readTestHeaders reads until \r\n\r\n with a deadline.
func readTestHeaders(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var data []byte
	buf := make([]byte, 4096)
	for !strings.Contains(string(data), "\r\n\r\n") {
		n, err := conn.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if err != nil {
			t.Fatalf("read headers: %v (got %q)", err, data)
		}
	}
	return data
}

// fakeUpstream runs one HTTP-proxy-style exchange then closes.
func fakeUpstream(t *testing.T, onRequest func(t *testing.T, req []byte) []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req := readTestHeaders(t, conn)
		resp := onRequest(t, req)
		_, _ = conn.Write(resp)
	}()
	return ln.Addr().String()
}

func TestBridgeStartStop(t *testing.T) {
	b := testBridge(t, "http://127.0.0.1:9999")
	if b.Endpoint() != "" {
		t.Error("Endpoint() should be empty before Start")
	}
	endpoint, err := b.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Errorf("endpoint = %q", endpoint)
	}
	if b.Endpoint() != endpoint {
		t.Error("Endpoint() mismatch after Start")
	}
	if again, err := b.Start(); err != nil || again != endpoint {
		t.Errorf("second Start = %q, %v; want idempotent %q", again, err, endpoint)
	}
	// Port is actually listening.
	c := dialTCP(t, strings.TrimPrefix(endpoint, "http://"))
	c.Close()
	b.Stop()
	if b.Endpoint() != "" {
		t.Error("Endpoint() should be empty after Stop")
	}
	b.Stop() // second stop must not hang or panic
}

func TestBridgeHTTPEndToEnd(t *testing.T) {
	upstream := fakeUpstream(t, func(t *testing.T, req []byte) []byte {
		if !strings.Contains(string(req), "GET http://example.com/ HTTP/1.1") {
			t.Errorf("upstream got wrong request line: %q", strings.SplitN(string(req), "\r\n", 2)[0])
		}
		return []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello")
	})
	b := testBridge(t, "http://"+upstream)
	endpoint, err := b.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	c := dialTCP(t, strings.TrimPrefix(endpoint, "http://"))
	_, _ = c.Write([]byte("GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n"))
	resp, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(resp), "200 OK") || !strings.Contains(string(resp), "hello") {
		t.Errorf("unexpected relayed response: %q", resp)
	}
}

func TestBridgeHTTPAuthInjection(t *testing.T) {
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	upstream := fakeUpstream(t, func(t *testing.T, req []byte) []byte {
		if !strings.Contains(string(req), "Proxy-Authorization: "+wantAuth) {
			t.Errorf("upstream missing Proxy-Authorization: %q", strings.SplitN(string(req), "\r\n\r\n", 2)[0])
		}
		return []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	})
	b := testBridge(t, "http://user:pass@"+upstream)
	endpoint, err := b.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	c := dialTCP(t, strings.TrimPrefix(endpoint, "http://"))
	_, _ = c.Write([]byte("GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n"))
	resp, _ := io.ReadAll(c)
	if !strings.Contains(string(resp), "200 OK") {
		t.Errorf("unexpected response: %q", resp)
	}
}

func TestBridgeCONNECTEndToEnd(t *testing.T) {
	upstream := fakeUpstream(t, func(t *testing.T, req []byte) []byte {
		if !strings.HasPrefix(string(req), "CONNECT example.com:443 HTTP/1.1") {
			t.Errorf("upstream got wrong CONNECT: %q", strings.SplitN(string(req), "\r\n", 2)[0])
		}
		// 200 + tunneled payload; bridge must forward both.
		return []byte("HTTP/1.1 200 Connection established\r\n\r\nTUNNEL-DATA")
	})
	b := testBridge(t, "http://"+upstream)
	endpoint, err := b.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	c := dialTCP(t, strings.TrimPrefix(endpoint, "http://"))
	_, _ = c.Write([]byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"))
	resp, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(resp), "200") {
		t.Errorf("missing 200 in CONNECT reply: %q", resp)
	}
	if !strings.Contains(string(resp), "TUNNEL-DATA") {
		t.Errorf("tunneled payload not relayed: %q", resp)
	}
}

func TestBridgeCONNECT407(t *testing.T) {
	upstream := fakeUpstream(t, func(t *testing.T, req []byte) []byte {
		return []byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
	})
	b := testBridge(t, "http://"+upstream)
	endpoint, err := b.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	c := dialTCP(t, strings.TrimPrefix(endpoint, "http://"))
	_, _ = c.Write([]byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"))
	resp, _ := io.ReadAll(c)
	if !strings.Contains(string(resp), "502 Bad Gateway") {
		t.Errorf("expected 502, got %q", resp)
	}
	if d := b.Diagnostic(); d == nil || d.Kind != "http_proxy_auth" {
		t.Errorf("diagnostic = %+v, want kind http_proxy_auth", d)
	}
}

func TestBridgeUpstreamDown(t *testing.T) {
	// Grab a port then close it so the dial is refused fast.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	b := testBridge(t, "http://"+dead)
	endpoint, err := b.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	c := dialTCP(t, strings.TrimPrefix(endpoint, "http://"))
	_, _ = c.Write([]byte("GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	resp, _ := io.ReadAll(c)
	if !strings.Contains(string(resp), "502 Bad Gateway") {
		t.Errorf("expected 502, got %q", resp)
	}
	if d := b.Diagnostic(); d == nil || d.Kind != "upstream_connect" {
		t.Errorf("diagnostic = %+v, want kind upstream_connect", d)
	}
}

// fakeSocks5Server speaks a minimal SOCKS5 handshake, then serves one HTTP
// exchange and closes.
func fakeSocks5Server(t *testing.T, onHTTP func(t *testing.T, req []byte) []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		fail := func(format string, args ...any) {
			t.Errorf("fake socks5: "+format, args...)
		}
		hdr, err := readExact(conn, 2)
		if err != nil {
			fail("greeting: %v", err)
			return
		}
		if _, err := readExact(conn, int(hdr[1])); err != nil {
			fail("methods: %v", err)
			return
		}
		if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
			return
		}
		h, err := readExact(conn, 4)
		if err != nil {
			fail("request head: %v", err)
			return
		}
		switch h[3] {
		case 0x01:
			if _, err := readExact(conn, 4); err != nil {
				fail("ipv4: %v", err)
				return
			}
		case 0x03:
			sz, err := readExact(conn, 1)
			if err != nil {
				return
			}
			if _, err := readExact(conn, int(sz[0])); err != nil {
				return
			}
		case 0x04:
			if _, err := readExact(conn, 16); err != nil {
				return
			}
		default:
			fail("unexpected atyp %d", h[3])
			return
		}
		if _, err := readExact(conn, 2); err != nil {
			fail("port: %v", err)
			return
		}
		if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
		req := readTestHeaders(t, conn)
		_, _ = conn.Write(onHTTP(t, req))
	}()
	return ln.Addr().String()
}

func TestBridgeSOCKS5EndToEnd(t *testing.T) {
	socks := fakeSocks5Server(t, func(t *testing.T, req []byte) []byte {
		// Bridge must have rewritten absolute-form to origin-form.
		if !strings.HasPrefix(string(req), "GET /hello HTTP/1.1\r\n") {
			t.Errorf("request not rewritten to origin-form: %q", strings.SplitN(string(req), "\r\n", 2)[0])
		}
		return []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
	})
	b := testBridge(t, "socks5://"+socks)
	endpoint, err := b.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	c := dialTCP(t, strings.TrimPrefix(endpoint, "http://"))
	// IP-literal target: no local DNS needed inside the sandbox.
	_, _ = c.Write([]byte("GET http://127.0.0.1:9999/hello HTTP/1.1\r\nHost: 127.0.0.1:9999\r\nConnection: close\r\n\r\n"))
	resp, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(resp), "200 OK") || !strings.HasSuffix(strings.TrimSpace(string(resp)), "ok") {
		t.Errorf("unexpected relayed response: %q", resp)
	}
}

func TestPrepareChromiumProxy(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		ep, br, err := PrepareChromiumProxy("", nil)
		if err != nil || ep != "" || br != nil {
			t.Errorf("got %q, %v, %v", ep, br, err)
		}
	})
	t.Run("plain passthrough", func(t *testing.T) {
		ep, br, err := PrepareChromiumProxy("http://127.0.0.1:8080", nil)
		if err != nil || br != nil {
			t.Fatalf("err=%v br=%v", err, br)
		}
		if ep != "http://127.0.0.1:8080" {
			t.Errorf("endpoint = %q", ep)
		}
	})
	t.Run("authenticated starts bridge", func(t *testing.T) {
		var logs []string
		ep, br, err := PrepareChromiumProxy("http://u:p@127.0.0.1:8080", func(s string) { logs = append(logs, s) })
		if err != nil || br == nil {
			t.Fatalf("err=%v br=%v", err, br)
		}
		defer br.Stop()
		if !strings.HasPrefix(ep, "http://127.0.0.1:") {
			t.Errorf("endpoint = %q, want local bridge", ep)
		}
		if len(logs) != 1 || !strings.Contains(logs[0], "authenticated proxy bridge") {
			t.Errorf("logs = %v", logs)
		}
	})
	t.Run("socks4a starts bridge", func(t *testing.T) {
		ep, br, err := PrepareChromiumProxy("socks4a://127.0.0.1:1080", nil)
		if err != nil || br == nil {
			t.Fatalf("err=%v br=%v", err, br)
		}
		defer br.Stop()
		if !strings.HasPrefix(ep, "http://127.0.0.1:") {
			t.Errorf("endpoint = %q", ep)
		}
	})
}

func TestPrepareHTTPCompatibleProxy(t *testing.T) {
	t.Run("plain http passthrough no bridge", func(t *testing.T) {
		ep, br, err := PrepareHTTPCompatibleProxy("http://127.0.0.1:8080", nil)
		if err != nil || br != nil {
			t.Fatalf("err=%v br=%v", err, br)
		}
		if ep != "http://127.0.0.1:8080" {
			t.Errorf("endpoint = %q", ep)
		}
	})
	t.Run("authenticated starts bridge", func(t *testing.T) {
		ep, br, err := PrepareHTTPCompatibleProxy("http://u:p@127.0.0.1:8080", nil)
		if err != nil || br == nil {
			t.Fatalf("err=%v br=%v", err, br)
		}
		defer br.Stop()
		if !strings.HasPrefix(ep, "http://127.0.0.1:") {
			t.Errorf("endpoint = %q", ep)
		}
	})
	t.Run("unsupported scheme", func(t *testing.T) {
		if _, _, err := PrepareHTTPCompatibleProxy("ftp://h:21", nil); err == nil {
			t.Error("expected error for ftp scheme")
		}
	})
	t.Run("empty", func(t *testing.T) {
		ep, br, err := PrepareHTTPCompatibleProxy("", nil)
		if err != nil || ep != "" || br != nil {
			t.Errorf("got %q, %v, %v", ep, br, err)
		}
	})
}

func TestWithHTTPCompatibleProxy(t *testing.T) {
	t.Run("passthrough", func(t *testing.T) {
		var seen string
		err := WithHTTPCompatibleProxy("http://127.0.0.1:8080", nil, func(ep string) error {
			seen = ep
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if seen != "http://127.0.0.1:8080" {
			t.Errorf("endpoint = %q", seen)
		}
	})
	t.Run("bridged and stopped", func(t *testing.T) {
		var seen string
		err := WithHTTPCompatibleProxy("socks5://127.0.0.1:1080", nil, func(ep string) error {
			seen = ep
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(seen, "http://127.0.0.1:") {
			t.Errorf("endpoint = %q", seen)
		}
	})
	t.Run("fn error propagates", func(t *testing.T) {
		want := io.ErrUnexpectedEOF
		err := WithHTTPCompatibleProxy("http://127.0.0.1:8080", nil, func(ep string) error {
			return want
		})
		if err != want {
			t.Errorf("err = %v, want %v", err, want)
		}
	})
}

func TestBridgeOpenProxySocketRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	b := testBridge(t, "http://"+dead)
	_, err = b.OpenProxySocket(context.Background())
	var be *BridgeError
	if !asBridgeError(err, &be) || be.Kind != "upstream_connect" {
		t.Errorf("err = %v, want BridgeError kind upstream_connect", err)
	}
}
