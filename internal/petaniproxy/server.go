package petaniproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultDialTimeout mirrors server.py's socket.create_connection timeout (4s).
const defaultDialTimeout = 4 * time.Second

// Server is a local rotating forward-proxy gateway. It accepts plain-HTTP
// proxy requests and HTTPS CONNECT tunnels, rotating each request across
// the PoolManager's upstream proxies with retry + sticky-session support.
type Server struct {
	addr        string
	pool        *PoolManager
	sanitizer   *Sanitizer
	dialTimeout time.Duration
	maxAttempts int

	httpSrv *http.Server
	mu      sync.Mutex
	ln      net.Listener
}

// Option configures a Server.
type Option func(*Server)

// WithProxies seeds the pool (replaces any existing proxies).
func WithProxies(p []Proxy) Option {
	return func(s *Server) { s.pool.SetProxies(p) }
}

// WithPoolManager uses an externally managed pool (shared with e.g. a
// health checker owned by the caller).
func WithPoolManager(pm *PoolManager) Option {
	return func(s *Server) {
		if pm != nil {
			s.pool = pm
		}
	}
}

// WithSessionTTL sets the sticky-session pin duration.
func WithSessionTTL(d time.Duration) Option {
	return func(s *Server) { s.pool.SetSessionTTL(d) }
}

// WithDialTimeout sets the upstream connect timeout (default 4s).
func WithDialTimeout(d time.Duration) Option {
	return func(s *Server) { s.dialTimeout = d }
}

// WithMaxAttempts caps upstream retries per client request (default 3).
func WithMaxAttempts(n int) Option {
	return func(s *Server) {
		if n >= 1 {
			s.maxAttempts = n
		}
	}
}

// WithUserAgents overrides the User-Agent rotation pool.
func WithUserAgents(uas []string) Option {
	return func(s *Server) { s.sanitizer.UserAgents = append([]string(nil), uas...) }
}

// NewServer builds a rotating gateway bound to addr (e.g. "127.0.0.1:8888").
func NewServer(addr string, opts ...Option) *Server {
	s := &Server{
		addr:        addr,
		pool:        NewPoolManager(nil),
		sanitizer:   NewSanitizer(),
		dialTimeout: defaultDialTimeout,
		maxAttempts: 3,
	}
	for _, o := range opts {
		o(s)
	}
	s.httpSrv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	return s
}

// SetProxies replaces the upstream pool (live, no restart needed).
func (s *Server) SetProxies(p []Proxy) { s.pool.SetProxies(p) }

// Pool exposes the underlying pool manager (rotation, stats, sessions).
func (s *Server) Pool() *PoolManager { return s.pool }

// Start binds the listener and serves in the background. It returns an error
// immediately if the address cannot be bound.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("petaniproxy: listen %s: %w", s.addr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	go func() {
		_ = s.httpSrv.Serve(ln) // ErrServerClosed on Stop is expected
	}()
	return nil
}

// Addr returns the actual bound address (useful when addr used port 0).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return s.addr
	}
	return s.ln.Addr().String()
}

// Stop gracefully shuts the server down.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.httpSrv.Shutdown(ctx)
}

// ServeHTTP routes: CONNECT -> tunnel, absolute-URI requests -> forward,
// anything else (direct hits on the gateway) -> 404. The dashboard HTML and
// REST API from server.py are intentionally not ported.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		s.handleConnect(w, r)
	case r.URL != nil && r.URL.IsAbs():
		s.handleForward(w, r)
	default:
		http.Error(w, "petaniproxy: not a proxy request", http.StatusNotFound)
	}
}

// extractSessionID mirrors server.py's extract_session_id: X-Session-ID /
// X-Sticky-Session header first, then ?session= / ?session_id= query param.
func extractSessionID(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Session-ID")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Sticky-Session")); v != "" {
		return v
	}
	q := r.URL.Query()
	if v := q.Get("session"); v != "" {
		return v
	}
	return q.Get("session_id")
}

// attempts returns how many upstream tries one client request gets:
// min(maxAttempts, pool size), at least 1 so an empty pool yields 502/504.
func (s *Server) attempts() int {
	n := s.pool.Size()
	if n > s.maxAttempts {
		n = s.maxAttempts
	}
	if n < 1 {
		n = 1 // pool empty: one Next() call returns ok=false -> 502/504
	}
	return n
}

// --- HTTPS CONNECT tunneling ---

// handleConnect mirrors server.py's do_CONNECT.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		http.Error(w, "petaniproxy: invalid CONNECT target", http.StatusBadRequest)
		return
	}
	sessionID := extractSessionID(r)
	for i := 0; i < s.attempts(); i++ {
		up, ok := s.pool.Next(sessionID)
		if !ok {
			break
		}
		uc, err := net.DialTimeout("tcp", up.Addr(), s.dialTimeout)
		if err != nil {
			s.pool.MarkResult(false)
			continue
		}
		if s.openTunnel(w, uc, up, target) {
			s.pool.MarkResult(true)
			return
		}
		s.pool.MarkResult(false)
	}
	http.Error(w, "petaniproxy: all upstream proxies failed (CONNECT)", http.StatusGatewayTimeout)
}

// openTunnel performs the upstream CONNECT handshake, hijacks the client
// connection, and relays bytes bidirectionally. False = try next upstream.
func (s *Server) openTunnel(w http.ResponseWriter, uc net.Conn, up Proxy, target string) bool {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if auth := proxyAuthHeader(up); auth != "" {
		fmt.Fprintf(&sb, "Proxy-Authorization: %s\r\n", auth)
	}
	sb.WriteString("\r\n")

	_ = uc.SetDeadline(time.Now().Add(s.dialTimeout))
	if _, err := io.WriteString(uc, sb.String()); err != nil {
		uc.Close()
		return false
	}
	br := bufio.NewReader(uc)
	if ok := readConnectOK(br); !ok {
		uc.Close()
		return false
	}
	_ = uc.SetDeadline(time.Time{})

	hj, ok := w.(http.Hijacker)
	if !ok {
		uc.Close()
		return false
	}
	cc, _, err := hj.Hijack()
	if err != nil {
		uc.Close()
		return false
	}
	if _, err := io.WriteString(cc, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		cc.Close()
		uc.Close()
		return false
	}
	relayConns(cc, &drainConn{Conn: uc, r: io.MultiReader(br, uc)})
	return true
}

// readConnectOK consumes the upstream CONNECT response head and reports
// whether the status code is 2xx. (server.py just checked `"200" in resp`.)
func readConnectOK(br *bufio.Reader) bool {
	line, err := br.ReadString('\n')
	if err != nil {
		return false
	}
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return false
	}
	if !strings.HasPrefix(parts[1], "2") {
		return false
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" || l == "\n" {
			break
		}
	}
	return true
}

// --- Plain HTTP forwarding ---

// handleForward mirrors server.py's handle_http_forward: the sanitized raw
// request (absolute URI, as sent by proxy clients) is written to the upstream
// proxy socket, then the upstream response is streamed back to the client.
func (s *Server) handleForward(w http.ResponseWriter, r *http.Request) {
	sessionID := extractSessionID(r)
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		http.Error(w, "petaniproxy: cannot read request body", http.StatusBadRequest)
		return
	}

	hdr := s.sanitizer.Sanitize(r.Header)
	if hdr.Get("Host") == "" {
		hdr.Set("Host", r.Host)
	}
	// Go's server already de-chunked the body; re-emit a correct length.
	if len(body) > 0 && hdr.Get("Content-Length") == "" {
		hdr.Set("Content-Length", strconv.Itoa(len(body)))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s %s\r\n", r.Method, r.RequestURI, r.Proto)
	for k, vv := range hdr {
		for _, v := range vv {
			fmt.Fprintf(&sb, "%s: %s\r\n", k, v)
		}
	}
	sb.WriteString("\r\n")
	rawHead := sb.String()

	for i := 0; i < s.attempts(); i++ {
		up, ok := s.pool.Next(sessionID)
		if !ok {
			break
		}
		if s.forwardOnce(w, r, up, rawHead, body) {
			s.pool.MarkResult(true)
			return
		}
		s.pool.MarkResult(false)
	}
	http.Error(w, "petaniproxy: all upstream proxies failed", http.StatusBadGateway)
}

// forwardOnce sends one raw request to a single upstream and streams the
// parsed response back. False = try the next upstream.
func (s *Server) forwardOnce(w http.ResponseWriter, r *http.Request, up Proxy, rawHead string, body []byte) bool {
	uc, err := net.DialTimeout("tcp", up.Addr(), s.dialTimeout)
	if err != nil {
		return false
	}
	defer uc.Close()

	_ = uc.SetDeadline(time.Now().Add(s.dialTimeout))
	head := rawHead
	if auth := proxyAuthHeader(up); auth != "" {
		head = strings.Replace(rawHead, "\r\n\r\n", "\r\nProxy-Authorization: "+auth+"\r\n\r\n", 1)
	}
	if _, err := io.WriteString(uc, head); err != nil {
		return false
	}
	if len(body) > 0 {
		if _, err := uc.Write(body); err != nil {
			return false
		}
	}
	_ = uc.SetDeadline(time.Time{})

	resp, err := http.ReadResponse(bufio.NewReader(uc), &http.Request{Method: r.Method})
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		if hopByHopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}

// proxyAuthHeader builds a Proxy-Authorization value when the upstream proxy
// carries credentials; empty otherwise.
func proxyAuthHeader(up Proxy) string {
	if up.Username == "" {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(up.Username+":"+up.Password))
}
