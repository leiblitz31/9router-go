package petaniproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// proxyFromListener builds a Proxy pointing at a local test listener.
func proxyFromListener(t *testing.T, ln net.Listener) Proxy {
	t.Helper()
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return Proxy{IP: host, Port: port}
}

// deadProxy returns a Proxy pointing at a port that is guaranteed closed.
func deadProxy(t *testing.T) Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := proxyFromListener(t, ln)
	ln.Close()
	return p
}

// startFakeConnectUpstream is a minimal CONNECT proxy: it answers 200 to
// CONNECT and then echoes everything (the "origin server").
func startFakeConnectUpstream(t *testing.T) (Proxy, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "CONNECT ") {
					_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
					return
				}
				for {
					l, err := br.ReadString('\n')
					if err != nil || l == "\r\n" || l == "\n" {
						break
					}
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				_, _ = io.Copy(c, c) // echo tunnel payload
			}(c)
		}
	}()
	return proxyFromListener(t, ln), func() { ln.Close() }
}

type capturedHTTP struct {
	method     string
	requestURI string
	proto      string
	header     http.Header
	body       []byte
}

// startFakeHTTPUpstream is a minimal forward proxy: it captures the raw
// request it received and answers 200 with body "hello".
func startFakeHTTPUpstream(t *testing.T, seen chan<- capturedHTTP, hits *atomic.Int64) (Proxy, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if hits != nil {
					hits.Add(1)
				}
				br := bufio.NewReader(c)
				reqLine, err := br.ReadString('\n')
				if err != nil {
					return
				}
				parts := strings.SplitN(strings.TrimSpace(reqLine), " ", 3)
				if len(parts) != 3 {
					return
				}
				hdr := http.Header{}
				for {
					l, err := br.ReadString('\n')
					if err != nil {
						return
					}
					l = strings.TrimRight(l, "\r\n")
					if l == "" {
						break
					}
					kv := strings.SplitN(l, ":", 2)
					if len(kv) == 2 {
						hdr.Add(strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1]))
					}
				}
				var body []byte
				if n, _ := strconv.Atoi(hdr.Get("Content-Length")); n > 0 {
					body = make([]byte, n)
					_, _ = io.ReadFull(br, body)
				}
				select {
				case seen <- capturedHTTP{method: parts[0], requestURI: parts[1], proto: parts[2], header: hdr, body: body}:
				default:
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-Echo: yes\r\nConnection: close\r\n\r\nhello")
			}(c)
		}
	}()
	return proxyFromListener(t, ln), func() { ln.Close() }
}

func startTestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	srv := NewServer("127.0.0.1:0", opts...)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv
}

func proxyClient(t *testing.T, srv *Server) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse("http://" + srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
}

func TestServerStartStop(t *testing.T) {
	srv := NewServer("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	addr := srv.Addr()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("Addr() = %q, want 127.0.0.1:<port>", addr)
	}
	if err := srv.Stop(); err != nil {
		t.Errorf("Stop(): %v", err)
	}
	if _, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		t.Error("dial succeeded after Stop(), want connection refused")
	}
}

func TestServerStartBindConflict(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := NewServer(ln.Addr().String())
	if err := srv.Start(); err == nil {
		_ = srv.Stop()
		t.Error("Start() on occupied port: want error, got nil")
	}
}

func TestConnectTunnel(t *testing.T) {
	up, stopUp := startFakeConnectUpstream(t)
	defer stopUp()
	srv := startTestServer(t, WithProxies([]Proxy{up}))

	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = fmt.Fprintf(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")

	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading CONNECT response: %v", err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", strings.TrimSpace(status))
	}
	if blank, _ := br.ReadString('\n'); strings.TrimSpace(blank) != "" {
		t.Fatalf("expected blank line after status, got %q", blank)
	}

	const payload = "tunnel-payload-123"
	if _, err := io.WriteString(c, payload); err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("reading tunnel echo: %v", err)
	}
	if string(echo) != payload {
		t.Errorf("tunnel echo = %q, want %q", echo, payload)
	}

	// Success is marked when the tunnel closes (mirrors server.py, which
	// marks after pipe_sockets returns).
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st := srv.Pool().Stats(); st.Successful == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Successful = %d, want 1 after tunnel close", srv.Pool().Stats().Successful)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConnectUpstreamRefused(t *testing.T) {
	srv := startTestServer(t,
		WithProxies([]Proxy{deadProxy(t)}),
		WithDialTimeout(300*time.Millisecond),
	)
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = fmt.Fprintf(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading error response: %v", err)
	}
	if !strings.Contains(status, "504") {
		t.Errorf("status = %q, want 504 when all upstreams fail", strings.TrimSpace(status))
	}
	if st := srv.Pool().Stats(); st.Failed != 1 {
		t.Errorf("Failed = %d, want 1", st.Failed)
	}
}

func TestHTTPForward(t *testing.T) {
	seen := make(chan capturedHTTP, 1)
	up, stopUp := startFakeHTTPUpstream(t, seen, nil)
	defer stopUp()
	srv := startTestServer(t, WithProxies([]Proxy{up}))
	client := proxyClient(t, srv)

	req, _ := http.NewRequest("GET", "http://example.com/some/path?x=1", nil)
	req.Header.Set("User-Agent", "python-requests/2.31.0")
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	req.Header.Set("Via", "1.1 evil")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello" {
		t.Errorf("response = %d %q, want 200 hello", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Echo") != "yes" {
		t.Error("upstream X-Echo header not relayed to client")
	}

	var cap capturedHTTP
	select {
	case cap = <-seen:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never received the forwarded request")
	}
	if cap.method != "GET" {
		t.Errorf("upstream method = %q, want GET", cap.method)
	}
	if cap.requestURI != "http://example.com/some/path?x=1" {
		t.Errorf("upstream requestURI = %q, want absolute URI", cap.requestURI)
	}
	ua := cap.header.Get("User-Agent")
	if ua == "python-requests/2.31.0" || !inStringList(CommonUserAgents, ua) {
		t.Errorf("upstream User-Agent = %q, want rotated pool UA", ua)
	}
	for _, k := range []string{"X-Forwarded-For", "Via"} {
		if v := cap.header.Get(k); v != "" {
			t.Errorf("upstream saw %s = %q, want stripped", k, v)
		}
	}
	if got := cap.header.Get("Host"); got != "example.com" {
		t.Errorf("upstream Host = %q, want example.com", got)
	}
}

func TestHTTPForwardPOST(t *testing.T) {
	seen := make(chan capturedHTTP, 1)
	up, stopUp := startFakeHTTPUpstream(t, seen, nil)
	defer stopUp()
	srv := startTestServer(t, WithProxies([]Proxy{up}))
	client := proxyClient(t, srv)

	resp, err := client.Post("http://example.com/submit", "text/plain", strings.NewReader("payload-123"))
	if err != nil {
		t.Fatalf("client.Post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello" {
		t.Errorf("response = %d %q, want 200 hello", resp.StatusCode, body)
	}
	select {
	case cap := <-seen:
		if cap.method != "POST" {
			t.Errorf("upstream method = %q, want POST", cap.method)
		}
		if string(cap.body) != "payload-123" {
			t.Errorf("upstream body = %q, want payload-123", cap.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never received the POST")
	}
}

func TestHTTPForwardRetriesDeadUpstream(t *testing.T) {
	seen := make(chan capturedHTTP, 1)
	alive, stopUp := startFakeHTTPUpstream(t, seen, nil)
	defer stopUp()
	srv := startTestServer(t,
		WithProxies([]Proxy{deadProxy(t), alive}), // dead first: must be skipped
		WithDialTimeout(300*time.Millisecond),
	)
	client := proxyClient(t, srv)

	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello" {
		t.Errorf("response = %d %q, want 200 hello via retried upstream", resp.StatusCode, body)
	}
	st := srv.Pool().Stats()
	if st.Failed < 1 || st.Successful < 1 {
		t.Errorf("stats = %+v, want >=1 failed (dead) and >=1 successful (alive)", st)
	}
}

func TestHTTPForwardEmptyPool(t *testing.T) {
	srv := startTestServer(t) // no proxies
	client := proxyClient(t, srv)
	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("status = %d, want 502 with empty pool", resp.StatusCode)
	}
}

func TestStickySessionThroughServer(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	seen := make(chan capturedHTTP, 8)
	upA, stopA := startFakeHTTPUpstream(t, seen, &hitsA)
	defer stopA()
	upB, stopB := startFakeHTTPUpstream(t, seen, &hitsB)
	defer stopB()
	srv := startTestServer(t, WithProxies([]Proxy{upA, upB}))
	client := proxyClient(t, srv)

	doReq := func(session string) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://example.com/", nil)
		req.Header.Set("X-Session-ID", session)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	doReq("sess-1")
	doReq("sess-1")
	doReq("sess-1")
	if hitsA.Load() != 3 || hitsB.Load() != 0 {
		t.Errorf("sticky: hitsA=%d hitsB=%d, want 3/0 (pinned to first upstream)", hitsA.Load(), hitsB.Load())
	}
	doReq("sess-2")
	if hitsB.Load() != 1 {
		t.Errorf("new session: hitsB=%d, want 1 (rotated to second upstream)", hitsB.Load())
	}
}

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		want string
	}{
		{
			name: "X-Session-ID header",
			req:  httptest.NewRequest("GET", "http://example.com/", nil),
			want: "",
		},
		{
			name: "X-Sticky-Session header",
			req:  httptest.NewRequest("CONNECT", "http://example.com:443", nil),
			want: "",
		},
		{
			name: "session query param",
			req:  httptest.NewRequest("GET", "http://example.com/path?session=abc123", nil),
			want: "abc123",
		},
		{
			name: "session_id query param",
			req:  httptest.NewRequest("GET", "http://example.com/path?session_id=xyz", nil),
			want: "xyz",
		},
		{
			name: "none",
			req:  httptest.NewRequest("GET", "http://example.com/path?a=b", nil),
			want: "",
		},
	}
	tests[0].req.Header.Set("X-Session-ID", "hdr-1")
	tests[0].want = "hdr-1"
	tests[1].req.Header.Set("X-Sticky-Session", "sticky-9")
	tests[1].want = "sticky-9"

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractSessionID(tt.req); got != tt.want {
				t.Errorf("extractSessionID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDirectRequestNotProxied(t *testing.T) {
	srv := startTestServer(t)
	// Direct origin-form request (no proxy): must not be forwarded.
	resp, err := http.Get("http://" + srv.Addr() + "/")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for non-proxy request", resp.StatusCode)
	}
}

func TestServerSetProxiesLive(t *testing.T) {
	seen := make(chan capturedHTTP, 1)
	up, stopUp := startFakeHTTPUpstream(t, seen, nil)
	defer stopUp()

	srv := startTestServer(t) // starts with empty pool
	client := proxyClient(t, srv)
	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502 before SetProxies", resp.StatusCode)
	}

	srv.SetProxies([]Proxy{up}) // live update, no restart
	resp2, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("client.Get after SetProxies: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("status = %d, want 200 after SetProxies", resp2.StatusCode)
	}
}

func TestProxyAuthHeader(t *testing.T) {
	tests := []struct {
		name  string
		proxy Proxy
		want  string
	}{
		{"no credentials", Proxy{IP: "1.2.3.4", Port: 8080}, ""},
		{"with credentials", Proxy{IP: "1.2.3.4", Port: 8080, Username: "user", Password: "pass"}, "Basic dXNlcjpwYXNz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := proxyAuthHeader(tt.proxy); got != tt.want {
				t.Errorf("proxyAuthHeader() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Ensure concurrent Next/MarkResult/Stats calls are race-free.
func TestPoolConcurrentAccess(t *testing.T) {
	pm := NewPoolManager(testProxies())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				pm.Next(fmt.Sprintf("sess-%d", i))
				pm.MarkResult(j%2 == 0)
				_ = pm.Stats()
			}
		}(i)
	}
	wg.Wait()
	if st := pm.Stats(); st.TotalRouted != 400 {
		t.Errorf("TotalRouted = %d, want 400", st.TotalRouted)
	}
}
