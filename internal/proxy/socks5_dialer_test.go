package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// SOCKS5 dial in the uTLS dialer (upstream #1120, fork adaptation).
//
// Upstream pins SOCKS5 pools by assigning t.Proxy, which the fork cannot do:
// a set t.Proxy makes net/http finish the handshake with crypto/tls and
// kills the JA4 fingerprint. The fork dialer therefore performs the SOCKS5
// handshake itself (RFC 1928, RFC 1929 auth) inside dialTunnel and keeps
// t.Proxy nil, so the TLS handshake still runs through uTLS DialTLSContext.
// These tests prove the dial is real — a live handshake against a minimal
// SOCKS5 server in-test — not just a URL recorded in the registry.

// socks5Request records one CONNECT the fake server received.
type socks5Request struct {
	atyp     byte // 0x01 IPv4, 0x03 domain, 0x04 IPv6
	host     string
	port     int
	username string
	password string
	methods  []byte
}

// fakeSocks5Server is a minimal RFC 1928 server: greeting, optional
// username/password auth (RFC 1929), CONNECT, then a byte splice to the
// requested target. It records every CONNECT so a test can assert what the
// dialer actually sent — address type (domain vs resolved IP) and auth.
type fakeSocks5Server struct {
	t           *testing.T
	ln          net.Listener
	addr        string
	mu          sync.Mutex
	requests    []socks5Request
	refuseReply byte // 0 = succeed; otherwise the REP code to refuse with
	requireAuth bool // only offer method 0x02 when the client offers it
}

func startFakeSocks5(t *testing.T, refuseReply byte, requireAuth bool) *fakeSocks5Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSocks5Server{t: t, ln: ln, addr: ln.Addr().String(), refuseReply: refuseReply, requireAuth: requireAuth}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return s
}

func (s *fakeSocks5Server) lastRequest() (socks5Request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return socks5Request{}, false
	}
	return s.requests[len(s.requests)-1], true
}

func (s *fakeSocks5Server) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	if hdr[0] != 0x05 {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	method := byte(0xff)
	if s.requireAuth {
		for _, m := range methods {
			if m == 0x02 {
				method = 0x02
				break
			}
		}
	} else {
		for _, m := range methods {
			if m == 0x00 {
				method = 0x00
				break
			}
		}
		if method == 0xff {
			for _, m := range methods {
				if m == 0x02 {
					method = 0x02
					break
				}
			}
		}
	}
	if _, err := c.Write([]byte{0x05, method}); err != nil {
		return
	}
	if method == 0xff {
		return
	}
	req := socks5Request{methods: append([]byte(nil), methods...)}
	if method == 0x02 {
		// RFC 1929 username/password subnegotiation.
		authHdr := make([]byte, 2)
		if _, err := io.ReadFull(br, authHdr); err != nil {
			return
		}
		ulen := make([]byte, authHdr[1])
		if _, err := io.ReadFull(br, ulen); err != nil {
			return
		}
		plenByte := make([]byte, 1)
		if _, err := io.ReadFull(br, plenByte); err != nil {
			return
		}
		passwd := make([]byte, plenByte[0])
		if _, err := io.ReadFull(br, passwd); err != nil {
			return
		}
		req.username, req.password = string(ulen), string(passwd)
		if _, err := c.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	}
	// CONNECT request: VER CMD RSV ATYP ADDR PORT.
	chead := make([]byte, 4)
	if _, err := io.ReadFull(br, chead); err != nil {
		return
	}
	if chead[0] != 0x05 || chead[1] != 0x01 {
		_, _ = c.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	req.atyp = chead[3]
	switch req.atyp {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		req.host = net.IP(b).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(br, l); err != nil {
			return
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		req.host = string(b)
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return
		}
		req.host = net.IP(b).String()
	default:
		_, _ = c.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(br, portBytes); err != nil {
		return
	}
	req.port = int(portBytes[0])<<8 | int(portBytes[1])

	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()

	if s.refuseReply != 0 {
		_, _ = c.Write([]byte{0x05, s.refuseReply, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	up, err := net.DialTimeout("tcp", net.JoinHostPort(req.host, fmt.Sprint(req.port)), 5*time.Second)
	if err != nil {
		_, _ = c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}
	go func() { _, _ = io.Copy(up, br) }()
	_, _ = io.Copy(c, up)
}

// startEchoServer runs a loopback TCP echo server and returns its address.
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// withLookupIPAddr swaps the package's local-resolution seam for one test,
// following the same pattern as withProxyResolver.
func withLookupIPAddr(t *testing.T, fn func(context.Context, string) ([]net.IPAddr, error)) {
	t.Helper()
	original := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = original })
	lookupIPAddr = fn
}

func TestDialTunnelSocks5(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	echoAddr := startEchoServer(t)
	_, echoPort, err := net.SplitHostPort(echoAddr)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		scheme      string // socks5 or socks5h
		proxyUser   string // "" or "user:pass"
		targetAddr  string
		targetHost  string
		requireAuth bool // fake server only offers method 0x02
		stubLookup  func(*testing.T)
		wantATYP    byte
		wantHost    string
		wantUser    string
		wantPass    string
	}{
		{
			name:       "socks5 no-auth dials target, IPv4 ATYP",
			scheme:     "socks5",
			targetAddr: echoAddr,
			targetHost: "127.0.0.1",
			wantATYP:   0x01,
			wantHost:   "127.0.0.1",
		},
		{
			name:       "socks5 resolves hostname locally",
			scheme:     "socks5",
			targetAddr: "localhost:" + echoPort,
			targetHost: "localhost",
			stubLookup: func(t *testing.T) {
				t.Helper()
				var lookedUp string
				withLookupIPAddr(t, func(_ context.Context, name string) ([]net.IPAddr, error) {
					lookedUp = name
					return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
				})
				t.Cleanup(func() {
					if lookedUp != "localhost" {
						t.Errorf("local lookup = %q, want localhost", lookedUp)
					}
				})
			},
			wantATYP: 0x01,
			wantHost: "127.0.0.1",
		},
		{
			name:       "socks5h sends domain, no local resolve",
			scheme:     "socks5h",
			targetAddr: "localhost:" + echoPort,
			targetHost: "localhost",
			stubLookup: func(t *testing.T) {
				t.Helper()
				withLookupIPAddr(t, func(_ context.Context, name string) ([]net.IPAddr, error) {
					t.Errorf("lookupIPAddr(%q) called for socks5h; the proxy must resolve remotely", name)
					return nil, errors.New("must not resolve locally")
				})
			},
			wantATYP: 0x03,
			wantHost: "localhost",
		},
		{
			name:        "socks5 with user:pass auth",
			scheme:      "socks5",
			proxyUser:   "alice:s3cret",
			targetAddr:  echoAddr,
			targetHost:  "127.0.0.1",
			requireAuth: true,
			wantATYP:    0x01,
			wantHost:    "127.0.0.1",
			wantUser:    "alice",
			wantPass:    "s3cret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.stubLookup != nil {
				tt.stubLookup(t)
			}
			socks := startFakeSocks5(t, 0, tt.requireAuth)
			proxyURL := tt.scheme + "://"
			if tt.proxyUser != "" {
				proxyURL += tt.proxyUser + "@"
			}
			proxyURL += socks.addr

			d := &utlsDialer{explicitProxyURL: proxyURL}
			conn, err := d.dialTunnel(ctx, "tcp", tt.targetAddr, tt.targetHost)
			if err != nil {
				t.Fatalf("dialTunnel via %s = %v, want a tunneled connection", proxyURL, err)
			}
			defer conn.Close()

			// Data must actually flow through the tunnel, not just connect.
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			const payload = "ping-socks"
			if _, err := conn.Write([]byte(payload)); err != nil {
				t.Fatalf("write through SOCKS tunnel = %v", err)
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("read through SOCKS tunnel = %v", err)
			}
			if string(got) != payload {
				t.Errorf("echo = %q, want %q", got, payload)
			}

			req, ok := socks.lastRequest()
			if !ok {
				t.Fatal("fake SOCKS server saw no CONNECT")
			}
			if req.atyp != tt.wantATYP {
				t.Errorf("ATYP = 0x%02x, want 0x%02x", req.atyp, tt.wantATYP)
			}
			if req.host != tt.wantHost {
				t.Errorf("CONNECT host = %q, want %q", req.host, tt.wantHost)
			}
			if req.port != mustPort(t, echoPort) {
				t.Errorf("CONNECT port = %d, want %s", req.port, echoPort)
			}
			if req.username != tt.wantUser || req.password != tt.wantPass {
				t.Errorf("auth = %q/%q, want %q/%q", req.username, req.password, tt.wantUser, tt.wantPass)
			}
		})
	}

	t.Run("socks5 refusal surfaces error", func(t *testing.T) {
		socks := startFakeSocks5(t, 0x05, false)
		d := &utlsDialer{explicitProxyURL: "socks5://" + socks.addr}
		_, err := d.dialTunnel(ctx, "tcp", echoAddr, "127.0.0.1")
		if err == nil {
			t.Fatal("dialTunnel through a refusing SOCKS server = nil, want an error")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "socks") {
			t.Errorf("error = %v, want it to name SOCKS", err)
		}
	})

	t.Run("socks5 default port is 1080", func(t *testing.T) {
		for _, scheme := range []string{"socks5", "socks5h"} {
			u := mustParseURL(t, scheme+"://proxy.example")
			if got := canonicalProxyAddr(u); got != "proxy.example:1080" {
				t.Errorf("canonicalProxyAddr(%s) = %q, want proxy.example:1080", scheme, got)
			}
		}
	})
}

func mustPort(t *testing.T, port string) int {
	t.Helper()
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		t.Fatalf("bad port %q: %v", port, err)
	}
	return p
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) = %v", raw, err)
	}
	return u
}

// TestDialTLSContextViaSocks5 is the end-to-end fingerprint proof: an HTTPS
// request through a SOCKS5 proxy must reach the upstream, the handshake must
// run through the dialer's DialTLSContext (counted, like the /tmp/proxyprobe
// check that caught the t.Proxy regression), and t.Proxy must stay nil —
// otherwise net/http would finish the handshake with crypto/tls and the
// JA4 fingerprint would be gone.
func TestDialTLSContextViaSocks5(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	// GetConfigForClient fires on every handshake, unlike GetCertificate,
	// which crypto/tls skips when the ClientHello carries no SNI and the
	// server already has a certificate — and no SNI is sent for an IP
	// literal (stdlib behaviour uTLS mirrors). Set it before StartTLS so
	// the clone StartTLS builds carries it.
	var mu sync.Mutex
	var hellosSeen int
	var helloSuites [][]uint16
	upstream.TLS = &tls.Config{
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			mu.Lock()
			defer mu.Unlock()
			hellosSeen++
			helloSuites = append(helloSuites, chi.CipherSuites)
			// The config StartTLS built (with the test certificate) is what
			// the handshake must use; returning it keeps the hook read-only.
			return upstream.TLS.Clone(), nil
		},
	}
	upstream.StartTLS()
	defer upstream.Close()

	socks := startFakeSocks5(t, 0, false)
	socksURL := "socks5://" + socks.addr

	tr := NewSafeTransportWithProxy(socksURL, true)
	if tr.Proxy != nil {
		t.Fatal("NewSafeTransportWithProxy installed t.Proxy; the uTLS dialer tunnels manually and must keep it nil")
	}
	var dialTLSCalls atomic.Int32
	origDialTLS := tr.DialTLSContext
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialTLSCalls.Add(1)
		return origDialTLS(ctx, network, addr)
	}
	if got := ProxyURLFor(&http.Client{Transport: tr}); got != socksURL {
		t.Fatalf("ProxyURLFor = %q, want %q", got, socksURL)
	}

	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := client.Get(upstream.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET through SOCKS5 proxy = %v, want 200", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read body = %v", err)
	}

	if dialTLSCalls.Load() == 0 {
		t.Error("DialTLSContext was never called; the handshake did not go through the uTLS dialer")
	}
	mu.Lock()
	defer mu.Unlock()
	if hellosSeen == 0 {
		t.Error("upstream saw no ClientHello; the TLS handshake never arrived")
	}
	for _, suites := range helloSuites {
		if len(suites) == 0 {
			t.Error("ClientHello offered no cipher suites")
		}
	}
	req, ok := socks.lastRequest()
	if !ok {
		t.Fatal("fake SOCKS server saw no CONNECT")
	}
	if req.atyp != 0x01 || req.host != "127.0.0.1" {
		t.Errorf("SOCKS CONNECT = ATYP 0x%02x host %q, want IPv4 127.0.0.1", req.atyp, req.host)
	}
}
