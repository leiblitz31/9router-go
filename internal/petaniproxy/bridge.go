// bridge.go - Go port of petani-proxy core/proxy_bridge.py.
//
// Shared local HTTP bridge for native proxy schemes: exposes an
// HTTP/HTTPS/SOCKS4/SOCKS4a/SOCKS5/SOCKS5h upstream as a localhost HTTP
// proxy, plus Chromium-compatible proxy preparation helpers.
//
// Behavioral parity notes (Python -> Go):
//   - socketserver.ThreadingTCPServer maps to net.Listener + a goroutine
//     per accepted connection; shutdown()/server_close() maps to Stop().
//   - The select()-based _relay maps to two copy goroutines with per-read
//     idle deadlines (90s of silence ends the relay, like select timeout).
//   - ProxyBridgeError(OSError) with .kind maps to *BridgeError; only
//     BridgeErrors produce the "502 Bad Gateway" reply, other failures only
//     record a diagnostic - exactly like _BridgeHandler.handle.
//   - Python ssl.SSLError in the handler has no direct Go equivalent; TLS
//     failures from the HTTPS-upstream wrap in OpenProxySocket are already
//     tagged "https_proxy_tls". Client-side TLS noise lands in "bridge".
//   - host.encode("idna") has no stdlib equivalent (x/net is off-limits);
//     idnaASCII passes ASCII hostnames through and rejects non-ASCII with a
//     "remote_dns" BridgeError, documented at the call sites.
//   - The http_compatible_proxy context manager maps to
//     WithHTTPCompatibleProxy(endpoint-func style), the idiomatic Go shape.
package petaniproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultBridgeTimeout mirrors the socket timeouts (20s) in proxy_bridge.py.
const DefaultBridgeTimeout = 20 * time.Second

// DefaultBridgeRelayTimeout mirrors the relay idle timeout (90s).
const DefaultBridgeRelayTimeout = 90 * time.Second

// maxBridgeHeaderBytes mirrors the 65536-byte header cap in _recv_until_headers.
const maxBridgeHeaderBytes = 65536

func isSupportedBridgeScheme(s string) bool {
	switch s {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
		return true
	}
	return false
}

// BridgeDiagnostic mirrors ProxyBridgeDiagnostic: the last failure recorded
// by a LocalProxyBridge. At is Unix seconds, parity with time.time().
type BridgeDiagnostic struct {
	Kind    string
	Message string
	At      float64
}

// BridgeError mirrors ProxyBridgeError: an error tagged with a
// machine-readable kind (e.g. "socks_auth", "upstream_connect").
type BridgeError struct {
	Kind string
	Msg  string
}

func (e *BridgeError) Error() string { return e.Kind + ": " + e.Msg }

// NewBridgeError builds a *BridgeError, stringifying msg like Python's %s.
func NewBridgeError(kind string, msg any) *BridgeError {
	return &BridgeError{Kind: kind, Msg: fmt.Sprint(msg)}
}

// ParseProxyURL mirrors parse_proxy_url: trims input and prepends "http://"
// when no scheme is present. Empty input yields (nil, nil), like Python's None.
func ParseProxyURL(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy.ParseProxyURL: %w", err)
	}
	return u, nil
}

// SafeProxyPort mirrors safe_proxy_port: the URL's port, or 0 when absent.
// A non-numeric explicit port returns an error, like parsed.port raising
// ValueError. (Go's url.Port() swallows invalid ports, so the host is
// inspected directly to preserve the error signal.)
func SafeProxyPort(u *url.URL) (int, error) {
	if u == nil {
		return 0, nil
	}
	host := u.Host
	if strings.HasPrefix(host, "[") {
		if i := strings.LastIndex(host, "]:"); i >= 0 {
			return atoiBridgePort(host[i+2:])
		}
		return 0, nil
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return atoiBridgePort(host[i+1:])
	}
	return 0, nil
}

func atoiBridgePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("petaniproxy.SafeProxyPort: invalid port %q", s)
	}
	return n, nil
}

// ProxyHasAuth mirrors proxy_has_auth: true when the URL carries a username
// or password (even an empty username with a password set).
func ProxyHasAuth(raw string) bool {
	u, err := ParseProxyURL(raw)
	if err != nil || u == nil || u.Hostname() == "" || u.User == nil {
		return false
	}
	if u.User.Username() != "" {
		return true
	}
	_, passwordSet := u.User.Password()
	return passwordSet
}

// LocalProxyBridge exposes HTTP/HTTPS/SOCKS upstreams as a localhost HTTP
// proxy. Mirrors the LocalProxyBridge class in proxy_bridge.py.
type LocalProxyBridge struct {
	UpstreamScheme string
	UpstreamHost   string
	UpstreamPort   int
	Username       string
	Password       string
	// AuthHeader is the base64 "user:pass" token, "" when no credentials.
	AuthHeader string

	Timeout      time.Duration
	RelayTimeout time.Duration

	mu         sync.Mutex
	listener   net.Listener
	localProxy string
	wg         sync.WaitGroup

	diagMu   sync.Mutex
	lastDiag *BridgeDiagnostic
}

// LocalAuthProxyBridge is an alias kept for parity with proxy_bridge.py
// (where LocalAuthProxyBridge = LocalProxyBridge).
type LocalAuthProxyBridge = LocalProxyBridge

func defaultBridgePort(scheme string) int {
	switch {
	case scheme == "https":
		return 443
	case strings.HasPrefix(scheme, "socks"):
		return 1080
	default:
		return 80
	}
}

// NewLocalProxyBridge parses proxyURL and builds a bridge, mirroring
// LocalProxyBridge.__init__ (ValueError becomes a Go error).
func NewLocalProxyBridge(proxyURL string) (*LocalProxyBridge, error) {
	u, err := ParseProxyURL(proxyURL)
	if err != nil || u == nil || u.Hostname() == "" {
		return nil, fmt.Errorf("petaniproxy.NewLocalProxyBridge: proxy URL is invalid")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	if !isSupportedBridgeScheme(scheme) {
		return nil, fmt.Errorf("petaniproxy.NewLocalProxyBridge: local proxy bridge supports HTTP, HTTPS, SOCKS4 and SOCKS5")
	}
	port, perr := SafeProxyPort(u)
	if perr != nil || port == 0 {
		port = defaultBridgePort(scheme)
	}
	var username, password string
	if u.User != nil {
		username = u.User.Username() // url.URL already percent-decodes, like unquote()
		if pw, set := u.User.Password(); set {
			password = pw
		}
	}
	authHeader := ""
	if username != "" || password != "" {
		authHeader = base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	}
	return &LocalProxyBridge{
		UpstreamScheme: scheme,
		UpstreamHost:   u.Hostname(),
		UpstreamPort:   port,
		Username:       username,
		Password:       password,
		AuthHeader:     authHeader,
		Timeout:        DefaultBridgeTimeout,
		RelayTimeout:   DefaultBridgeRelayTimeout,
	}, nil
}

// RecordDiagnostic stores the latest failure, truncating the message to 500
// chars like ProxyBridgeDiagnostic.
func (b *LocalProxyBridge) RecordDiagnostic(kind, message string) {
	if len(message) > 500 {
		message = message[:500]
	}
	b.diagMu.Lock()
	defer b.diagMu.Unlock()
	b.lastDiag = &BridgeDiagnostic{
		Kind:    kind,
		Message: message,
		At:      float64(time.Now().UnixNano()) / 1e9,
	}
}

// Diagnostic returns a copy of the last recorded diagnostic, or nil when
// none was recorded (mirrors diagnostic() returning None).
func (b *LocalProxyBridge) Diagnostic() *BridgeDiagnostic {
	b.diagMu.Lock()
	defer b.diagMu.Unlock()
	if b.lastDiag == nil {
		return nil
	}
	c := *b.lastDiag
	return &c
}

// IsHTTPUpstream mirrors the is_http_upstream property.
func (b *LocalProxyBridge) IsHTTPUpstream() bool {
	return b.UpstreamScheme == "http" || b.UpstreamScheme == "https"
}

// OpenProxySocket dials the HTTP/HTTPS upstream, wrapping HTTPS in TLS with
// system roots (mirrors ssl.create_default_context). Failures are tagged
// "upstream_connect" / "https_proxy_tls".
func (b *LocalProxyBridge) OpenProxySocket(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: b.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(b.UpstreamHost, strconv.Itoa(b.UpstreamPort)))
	if err != nil {
		return nil, NewBridgeError("upstream_connect", err)
	}
	if b.UpstreamScheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: b.UpstreamHost,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			return nil, NewBridgeError("https_proxy_tls", err)
		}
		conn = tlsConn
	}
	return conn, nil
}

// readUntilHeaders mirrors _recv_until_headers: reads until the header
// terminator or limit. EOF returns what was read (no error); a read timeout
// returns the timeout error like Python's socket.timeout propagating.
func readUntilHeaders(conn net.Conn, timeout time.Duration, limit int) ([]byte, error) {
	var data []byte
	for !bytes.Contains(data, []byte("\r\n\r\n")) && len(data) < limit {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return data, err
		}
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				return data, nil
			}
			return data, err
		}
	}
	return data, nil
}

// readExact mirrors _recv_exact: "remote_reset" BridgeError on EOF.
func readExact(conn net.Conn, n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, NewBridgeError("remote_reset", "unexpected EOF from proxy")
	}
	return buf, nil
}

// bridgeRelayConns mirrors _relay from proxy_bridge.py: bidirectional copy
// until EOF or an idle timeout with no data in either direction, then closes
// both sides. (Named bridge* to avoid the sibling relay.go's relayConns,
// which ports server.py's pipe_sockets with different EOF-only semantics.)
func bridgeRelayConns(a, b net.Conn, timeout time.Duration) {
	var wg sync.WaitGroup
	wg.Add(2)
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			a.Close()
			b.Close()
		})
	}
	go func() {
		defer wg.Done()
		defer closeBoth()
		bridgeRelayOneWay(b, a, timeout)
	}()
	go func() {
		defer wg.Done()
		defer closeBoth()
		bridgeRelayOneWay(a, b, timeout)
	}()
	wg.Wait()
}

func bridgeRelayOneWay(dst, src net.Conn, timeout time.Duration) {
	buf := make([]byte, 65536)
	for {
		_ = src.SetReadDeadline(time.Now().Add(timeout))
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// splitHostPort mirrors _split_host_port. Invalid IPv6 yields a "target"
// BridgeError; a non-numeric port yields a plain error (Python's int()
// ValueError propagated to the generic "bridge" diagnostic).
func splitHostPort(value string, defaultPort int) (string, int, error) {
	text := strings.TrimSpace(value)
	if strings.HasPrefix(text, "[") {
		end := strings.Index(text, "]")
		if end < 0 {
			return "", 0, NewBridgeError("target", "invalid IPv6 target")
		}
		host := text[1:end]
		suffix := text[end+1:]
		if strings.HasPrefix(suffix, ":") {
			p, err := strconv.Atoi(suffix[1:])
			if err != nil {
				return "", 0, fmt.Errorf("petaniproxy.splitHostPort: invalid port %q", suffix[1:])
			}
			return host, p, nil
		}
		return host, defaultPort, nil
	}
	if strings.Count(text, ":") == 1 {
		host, portStr, _ := strings.Cut(text, ":")
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return "", 0, fmt.Errorf("petaniproxy.splitHostPort: invalid port %q", portStr)
		}
		return host, p, nil
	}
	return text, defaultPort, nil
}

// rewriteHTTPRequest mirrors _rewrite_http_request: converts an absolute-form
// request line to origin-form (dropping Proxy-Authorization) and resolves
// the target host/port from the URI or the Host header.
func rewriteHTTPRequest(initial []byte) (host string, port int, data []byte, err error) {
	parts := bytes.SplitN(initial, []byte("\r\n\r\n"), 2)
	head := parts[0]
	var body []byte
	if len(parts) == 2 {
		body = parts[1]
	}
	lines := bytes.Split(head, []byte("\r\n"))
	fields := strings.SplitN(string(lines[0]), " ", 3)
	if len(fields) != 3 {
		return "", 0, nil, NewBridgeError("request", "invalid HTTP request line")
	}
	method, target, version := fields[0], fields[1], fields[2]
	if u, perr := url.Parse(target); perr == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" {
		host = u.Hostname()
		port = 80
		if u.Scheme == "https" {
			port = 443
		}
		if p := u.Port(); p != "" {
			n, aerr := strconv.Atoi(p)
			if aerr != nil {
				// Python's parsed.port ValueError propagated to generic "bridge".
				return "", 0, nil, fmt.Errorf("petaniproxy.rewriteHTTPRequest: invalid port %q", p)
			}
			port = n
		}
		path := u.EscapedPath()
		if path == "" {
			path = "/"
		}
		if u.RawQuery != "" {
			path += "?" + u.RawQuery
		}
		lines[0] = []byte(method + " " + path + " " + version)
	} else {
		var hostHeader string
		for _, line := range lines[1:] {
			if len(line) > 5 && strings.EqualFold(string(line[:5]), "host:") {
				hostHeader = strings.TrimSpace(string(line[5:]))
				break
			}
		}
		if host, port, err = splitHostPort(hostHeader, 80); err != nil {
			return "", 0, nil, err
		}
	}
	var filtered [][]byte
	for _, line := range lines {
		if len(line) > 20 && strings.EqualFold(string(line[:20]), "proxy-authorization:") {
			continue
		}
		filtered = append(filtered, line)
	}
	data = bytes.Join(filtered, []byte("\r\n"))
	data = append(data, "\r\n\r\n"...)
	data = append(data, body...)
	return host, port, data, nil
}

// idnaASCII is the stdlib-only stand-in for host.encode("idna"): ASCII
// hostnames pass through unchanged; non-ASCII is rejected because
// golang.org/x/net/idna is off-limits (stdlib only).
func idnaASCII(host string) ([]byte, error) {
	for i := 0; i < len(host); i++ {
		if host[i] > 127 {
			return nil, NewBridgeError("remote_dns", "non-ASCII hostname requires IDNA encoding")
		}
	}
	return []byte(host), nil
}

// socks5Connect performs the SOCKS5/SOCKS5h handshake against an already
// connected socket. socks5 resolves non-IP hosts locally (like Python's
// getaddrinfo branch); socks5h sends the hostname for remote DNS.
func (b *LocalProxyBridge) socks5Connect(conn net.Conn, host string, port int) error {
	if port < 0 || port > 65535 {
		// Python's struct.pack("!H") raised here -> generic "bridge".
		return fmt.Errorf("petaniproxy.socks5Connect: port %d out of range", port)
	}
	methods := []byte{0x00}
	if b.Username != "" || b.Password != "" {
		methods = append([]byte{0x02}, methods...)
	}
	hello := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(hello); err != nil {
		return fmt.Errorf("petaniproxy.socks5Connect: greeting write: %w", err)
	}
	resp, err := readExact(conn, 2)
	if err != nil {
		return err
	}
	if resp[0] != 0x05 || resp[1] == 0xFF {
		return NewBridgeError("socks_auth", "SOCKS5 authentication method rejected")
	}
	if resp[1] == 0x02 {
		ub := []byte(b.Username)
		pb := []byte(b.Password)
		if len(ub) > 255 || len(pb) > 255 {
			return NewBridgeError("configuration", "SOCKS5 credentials too long")
		}
		authReq := append([]byte{0x01, byte(len(ub))}, ub...)
		authReq = append(authReq, byte(len(pb)))
		authReq = append(authReq, pb...)
		if _, err := conn.Write(authReq); err != nil {
			return fmt.Errorf("petaniproxy.socks5Connect: auth write: %w", err)
		}
		authResp, err := readExact(conn, 2)
		if err != nil {
			return err
		}
		if authResp[0] != 0x01 || authResp[1] != 0x00 {
			return NewBridgeError("socks_auth", "SOCKS5 authentication failed")
		}
	}
	var encoded []byte
	if addr, perr := netip.ParseAddr(host); perr == nil {
		if addr.Is4() {
			a4 := addr.As4()
			encoded = append([]byte{0x01}, a4[:]...)
		} else {
			a16 := addr.As16()
			encoded = append([]byte{0x04}, a16[:]...)
		}
	} else if b.UpstreamScheme == "socks5" {
		addrs, rerr := net.DefaultResolver.LookupIPAddr(context.Background(), host)
		if rerr != nil || len(addrs) == 0 {
			return NewBridgeError("local_dns", rerr)
		}
		if v4 := addrs[0].IP.To4(); v4 != nil {
			encoded = append([]byte{0x01}, v4...)
		} else {
			encoded = append([]byte{0x04}, addrs[0].IP.To16()...)
		}
	} else {
		rawHost, ierr := idnaASCII(host)
		if ierr != nil {
			return ierr
		}
		if len(rawHost) > 255 {
			return NewBridgeError("remote_dns", "SOCKS5 hostname too long")
		}
		encoded = append([]byte{0x03, byte(len(rawHost))}, rawHost...)
	}
	req := append([]byte{0x05, 0x01, 0x00}, encoded...)
	var portBuf [2]byte
	binary.BigEndian.PutUint16(portBuf[:], uint16(port))
	req = append(req, portBuf[:]...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("petaniproxy.socks5Connect: connect write: %w", err)
	}
	head, err := readExact(conn, 4)
	if err != nil {
		return err
	}
	if head[0] != 0x05 || head[1] != 0x00 {
		return NewBridgeError("socks_connect", fmt.Sprintf("SOCKS5 connect failed with status %d", head[1]))
	}
	switch head[3] {
	case 0x01:
		if _, err := readExact(conn, 4); err != nil {
			return err
		}
	case 0x04:
		if _, err := readExact(conn, 16); err != nil {
			return err
		}
	case 0x03:
		sz, err := readExact(conn, 1)
		if err != nil {
			return err
		}
		if _, err := readExact(conn, int(sz[0])); err != nil {
			return err
		}
	default:
		return NewBridgeError("socks_connect", "SOCKS5 returned invalid address type")
	}
	if _, err := readExact(conn, 2); err != nil {
		return err
	}
	return nil
}

// socks4Connect performs the SOCKS4/SOCKS4a handshake. socks4a sends the
// hostname for remote DNS (0.0.0.1 marker); socks4 resolves locally.
func (b *LocalProxyBridge) socks4Connect(conn net.Conn, host string, port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("petaniproxy.socks4Connect: port %d out of range", port)
	}
	remoteDNS := b.UpstreamScheme == "socks4a"
	var address net.IP
	if addr, perr := netip.ParseAddr(host); perr == nil && addr.Is4() {
		a4 := addr.As4()
		address = net.IP(a4[:])
	} else if remoteDNS {
		address = net.IP{0, 0, 0, 1}
	} else {
		addrs, rerr := net.DefaultResolver.LookupIPAddr(context.Background(), host)
		if rerr != nil || len(addrs) == 0 {
			return NewBridgeError("local_dns", rerr)
		}
		if v4 := addrs[0].IP.To4(); v4 != nil {
			address = v4
		} else {
			return NewBridgeError("local_dns", "no IPv4 address for "+host)
		}
	}
	payload := []byte{0x04, 0x01, byte(port >> 8), byte(port)}
	payload = append(payload, address.To4()...)
	payload = append(payload, []byte(b.Username)...)
	payload = append(payload, 0x00)
	if remoteDNS && address.Equal(net.IP{0, 0, 0, 1}) {
		rawHost, ierr := idnaASCII(host)
		if ierr != nil {
			return ierr
		}
		payload = append(payload, rawHost...)
		payload = append(payload, 0x00)
	}
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("petaniproxy.socks4Connect: write: %w", err)
	}
	resp, err := readExact(conn, 8)
	if err != nil {
		return err
	}
	if resp[1] != 0x5A {
		return NewBridgeError("socks_connect", fmt.Sprintf("SOCKS4 connect failed with status %d", resp[1]))
	}
	return nil
}

// OpenSocksTarget dials the SOCKS upstream and completes the handshake for
// the given target host/port, mirroring open_socks_target. The socket is
// closed on handshake failure.
func (b *LocalProxyBridge) OpenSocksTarget(ctx context.Context, host string, port int) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: b.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(b.UpstreamHost, strconv.Itoa(b.UpstreamPort)))
	if err != nil {
		return nil, NewBridgeError("upstream_connect", err)
	}
	// Handshake reads get the bridge timeout (Python set sock timeout first).
	_ = conn.SetReadDeadline(time.Now().Add(b.Timeout))
	var handshakeErr error
	if b.UpstreamScheme == "socks5" || b.UpstreamScheme == "socks5h" {
		handshakeErr = b.socks5Connect(conn, host, port)
	} else {
		handshakeErr = b.socks4Connect(conn, host, port)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if handshakeErr != nil {
		conn.Close()
		return nil, handshakeErr
	}
	return conn, nil
}

// InjectProxyAuth inserts a Proxy-Authorization header into raw HTTP request
// bytes when the upstream needs auth, mirroring inject_proxy_auth.
func (b *LocalProxyBridge) InjectProxyAuth(data []byte) []byte {
	if b.AuthHeader == "" || !bytes.Contains(data, []byte("\r\n\r\n")) {
		return data
	}
	if bytes.Contains(bytes.ToLower(data), []byte("\r\nproxy-authorization:")) {
		return data
	}
	parts := bytes.SplitN(data, []byte("\r\n\r\n"), 2)
	out := append(parts[0], "\r\n"...)
	out = append(out, "Proxy-Authorization: Basic "+b.AuthHeader...)
	out = append(out, "\r\n\r\n"...)
	out = append(out, parts[1]...)
	return out
}

// writeBridgeError classifies err like _BridgeHandler.handle's except chain:
// BridgeError -> record kind + 502 reply; DNS -> "local_dns"; timeouts and
// refused connections -> "upstream_connect"; everything else -> "bridge".
func (b *LocalProxyBridge) writeBridgeError(client net.Conn, err error) {
	var be *BridgeError
	if errors.As(err, &be) {
		b.RecordDiagnostic(be.Kind, be.Error())
		_, _ = client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		b.RecordDiagnostic("local_dns", err.Error())
		return
	}
	var netErr net.Error
	if errors.Is(err, syscall.ECONNREFUSED) || (errors.As(err, &netErr) && netErr.Timeout()) {
		b.RecordDiagnostic("upstream_connect", err.Error())
		return
	}
	b.RecordDiagnostic("bridge", err.Error())
}

// handleConn serves one downstream client connection, mirroring
// _BridgeHandler.handle (CONNECT tunneling plus plain HTTP forwarding).
func (b *LocalProxyBridge) handleConn(client net.Conn) {
	defer client.Close()
	var upstream net.Conn
	defer func() {
		if upstream != nil {
			upstream.Close()
		}
	}()
	ctx := context.Background()

	initial, err := readUntilHeaders(client, b.Timeout, maxBridgeHeaderBytes)
	if err != nil {
		b.RecordDiagnostic("bridge", err.Error())
		return
	}
	if len(initial) == 0 {
		return
	}
	firstLine := string(bytes.SplitN(initial, []byte("\r\n"), 2)[0])
	if strings.HasPrefix(strings.ToUpper(firstLine), "CONNECT ") {
		fields := strings.Fields(firstLine)
		if len(fields) < 2 {
			b.RecordDiagnostic("bridge", "invalid CONNECT request line")
			return
		}
		target := fields[1]
		if b.IsHTTPUpstream() {
			upstream, err = b.OpenProxySocket(ctx)
			if err != nil {
				b.writeBridgeError(client, err)
				return
			}
			var req strings.Builder
			req.WriteString("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n")
			if b.AuthHeader != "" {
				req.WriteString("Proxy-Authorization: Basic " + b.AuthHeader + "\r\n")
			}
			req.WriteString("\r\n")
			if _, err := io.WriteString(upstream, req.String()); err != nil {
				b.writeBridgeError(client, err)
				return
			}
			resp, err := readUntilHeaders(upstream, b.Timeout, maxBridgeHeaderBytes)
			if err != nil {
				b.writeBridgeError(client, err)
				return
			}
			status := resp
			if i := bytes.Index(resp, []byte("\r\n")); i >= 0 {
				status = resp[:i]
			}
			if !bytes.Contains(status, []byte(" 200 ")) {
				code := string(status)
				kind := "http_connect"
				if bytes.Contains(status, []byte(" 407 ")) {
					kind = "http_proxy_auth"
				}
				if code == "" {
					code = "proxy CONNECT failed"
				}
				b.writeBridgeError(client, NewBridgeError(kind, code))
				return
			}
			_, _ = client.Write(resp)
		} else {
			host, port, serr := splitHostPort(target, 443)
			if serr != nil {
				b.writeBridgeError(client, serr)
				return
			}
			upstream, err = b.OpenSocksTarget(ctx, host, port)
			if err != nil {
				b.writeBridgeError(client, err)
				return
			}
			_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\nProxy-Agent: local-bridge\r\n\r\n")
		}
		bridgeRelayConns(client, upstream, b.RelayTimeout)
		return
	}
	if b.IsHTTPUpstream() {
		upstream, err = b.OpenProxySocket(ctx)
		if err != nil {
			b.writeBridgeError(client, err)
			return
		}
		if _, err := upstream.Write(b.InjectProxyAuth(initial)); err != nil {
			b.writeBridgeError(client, err)
			return
		}
	} else {
		host, port, reqData, rerr := rewriteHTTPRequest(initial)
		if rerr != nil {
			b.writeBridgeError(client, rerr)
			return
		}
		upstream, err = b.OpenSocksTarget(ctx, host, port)
		if err != nil {
			b.writeBridgeError(client, err)
			return
		}
		if _, err := upstream.Write(reqData); err != nil {
			b.writeBridgeError(client, err)
			return
		}
	}
	bridgeRelayConns(client, upstream, b.RelayTimeout)
}

// Start binds 127.0.0.1 on an ephemeral port and serves the bridge,
// returning the local proxy URL (mirrors start()). Idempotent.
func (b *LocalProxyBridge) Start() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listener != nil {
		return b.localProxy, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("petaniproxy.LocalProxyBridge.Start: %w", err)
	}
	b.listener = ln
	b.localProxy = "http://127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	b.wg.Add(1)
	go b.serve(ln)
	return b.localProxy, nil
}

// serve runs the accept loop on ln, passed as a parameter so a Stop() that
// clears b.listener cannot race a goroutine that has not been scheduled yet.
func (b *LocalProxyBridge) serve(ln net.Listener) {
	defer b.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go func(c net.Conn) {
			defer b.wg.Done()
			b.handleConn(c)
		}(conn)
	}
}

// Stop shuts the bridge down (mirrors stop()).
func (b *LocalProxyBridge) Stop() {
	b.mu.Lock()
	ln := b.listener
	b.listener = nil
	b.localProxy = ""
	b.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	b.wg.Wait()
}

// Endpoint returns the current local proxy URL, or "" when stopped.
func (b *LocalProxyBridge) Endpoint() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.localProxy
}

// ProxyForChromium mirrors proxy_for_chromium: normalizes a proxy URL to
// Chromium's --proxy-server flag format. Authenticated proxies and SOCKS4A
// raise an error (they need PrepareChromiumProxy to preserve semantics).
func ProxyForChromium(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if ProxyHasAuth(s) {
		return "", fmt.Errorf("petaniproxy.ProxyForChromium: authenticated proxy requires PrepareChromiumProxy()")
	}
	u, err := ParseProxyURL(s)
	if err != nil || u == nil || u.Hostname() == "" {
		return "", nil
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	if scheme == "socks4a" {
		return "", fmt.Errorf("petaniproxy.ProxyForChromium: SOCKS4A requires PrepareChromiumProxy() to preserve remote DNS semantics")
	}
	if scheme == "socks5h" {
		scheme = "socks5"
	}
	host := u.Hostname()
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	port, perr := SafeProxyPort(u)
	if perr != nil || port == 0 {
		port = defaultBridgePort(scheme)
	}
	return scheme + "://" + host + ":" + strconv.Itoa(port), nil
}

// PrepareChromiumProxy mirrors prepare_chromium_proxy: returns a Chromium
// --proxy-server value plus the bridge to Stop() when one was started
// (authenticated or SOCKS4A proxies). A nil log discards messages.
func PrepareChromiumProxy(raw string, log func(string)) (string, *LocalProxyBridge, error) {
	if log == nil {
		log = func(string) {}
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil, nil
	}
	u, err := ParseProxyURL(s)
	if err != nil || u == nil || u.Hostname() == "" {
		return "", nil, fmt.Errorf("petaniproxy.PrepareChromiumProxy: proxy URL is invalid")
	}
	scheme := strings.ToLower(u.Scheme)
	if ProxyHasAuth(s) || scheme == "socks4a" {
		bridge, berr := NewLocalProxyBridge(s)
		if berr != nil {
			return "", nil, berr
		}
		endpoint, serr := bridge.Start()
		if serr != nil {
			return "", nil, serr
		}
		if ProxyHasAuth(s) {
			log("started authenticated proxy bridge: " + endpoint)
		} else {
			log("started SOCKS4A compatibility proxy bridge: " + endpoint)
		}
		return endpoint, bridge, nil
	}
	endpoint, cerr := ProxyForChromium(s)
	if cerr != nil {
		return "", nil, cerr
	}
	return endpoint, nil, nil
}

// PrepareHTTPCompatibleProxy mirrors prepare_http_compatible_proxy: returns
// a plain-HTTP proxy endpoint (starting a bridge when the upstream needs
// adaptation) plus the bridge to Stop(). A nil log discards messages.
func PrepareHTTPCompatibleProxy(raw string, log func(string)) (string, *LocalProxyBridge, error) {
	if log == nil {
		log = func(string) {}
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil, nil
	}
	u, err := ParseProxyURL(s)
	if err != nil || u == nil || u.Hostname() == "" {
		return "", nil, fmt.Errorf("petaniproxy.PrepareHTTPCompatibleProxy: proxy URL is invalid")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	if !isSupportedBridgeScheme(scheme) {
		return "", nil, fmt.Errorf("petaniproxy.PrepareHTTPCompatibleProxy: HTTP-compatible proxy endpoint does not support scheme: %s", scheme)
	}
	if scheme == "http" && !ProxyHasAuth(s) {
		return s, nil, nil
	}
	bridge, berr := NewLocalProxyBridge(s)
	if berr != nil {
		return "", nil, berr
	}
	endpoint, serr := bridge.Start()
	if serr != nil {
		return "", nil, serr
	}
	log("started local HTTP proxy endpoint: " + s + " -> " + endpoint)
	return endpoint, bridge, nil
}

// WithHTTPCompatibleProxy mirrors the http_compatible_proxy context manager:
// prepares an HTTP-compatible endpoint, invokes fn with it, then stops any
// bridge that was started.
func WithHTTPCompatibleProxy(raw string, log func(string), fn func(endpoint string) error) error {
	endpoint, bridge, err := PrepareHTTPCompatibleProxy(raw, log)
	if err != nil {
		return err
	}
	if bridge != nil {
		defer bridge.Stop()
	}
	return fn(endpoint)
}
