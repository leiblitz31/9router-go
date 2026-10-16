package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// TLS fingerprint spoofing for outbound HTTPS.
//
// Go's crypto/tls emits a recognizable JA3 fingerprint, which lets a provider
// distinguish gateway traffic from a genuine client. This dialer performs the
// TLS handshake with a spoofed ClientHello (refraction-networking/utls) while
// preserving everything else the gateway relies on:
//
//   - SSRF protection: the underlying TCP dial still goes through NewSafeDialer,
//     so private/reserved IPs stay blocked.
//   - Proxying: net/http would finish a proxied handshake with crypto/tls
//     (Transport.dialConn/addTLS), defeating the spoof. The dialer therefore
//     tunnels through the proxy itself (CONNECT) and keeps Transport.Proxy nil.
//     Environment proxy semantics are replicated exactly by consulting
//     http.ProxyFromEnvironment with a synthetic request.
//   - Introspection: transportProxyAssignments records how each transport dials,
//     so proxiesViaAssignment / proxyURLFor keep working without t.Proxy.
//
// The negotiated ALPN is pinned to http/1.1: the Transport cannot drive HTTP/2
// over a non-*tls.Conn, and every upstream API speaks h1 fine.

// transportProxyKind describes how a transport reaches the network.
type transportProxyKind int

const (
	proxyKindNone transportProxyKind = iota
	proxyKindEnv
	proxyKindExplicit
)

// transportProxyAssignment records a transport's egress choice for
// proxiesViaAssignment / proxyURLFor.
type transportProxyAssignment struct {
	kind transportProxyKind
	url  string // explicit proxy URL; "" for env/none
}

// transportProxyAssignments maps *http.Transport to its egress choice.
// Transports are long-lived and cached (proxyClients, insecureClients), so the
// entries live as long as the transports they describe.
var transportProxyAssignments sync.Map // map[*http.Transport]transportProxyAssignment

func registerTransportProxy(t *http.Transport, kind transportProxyKind, url string) {
	transportProxyAssignments.Store(t, transportProxyAssignment{kind: kind, url: url})
}

func lookupTransportProxy(t *http.Transport) transportProxyAssignment {
	if v, ok := transportProxyAssignments.Load(t); ok {
		if a, ok := v.(transportProxyAssignment); ok {
			return a
		}
	}
	return transportProxyAssignment{kind: proxyKindNone}
}

// utlsDialer dials TLS with a spoofed ClientHello.
type utlsDialer struct {
	// explicitProxyURL pins the dialer to one proxy. "" means no pin.
	explicitProxyURL string
	// useEnvProxy consults HTTPS_PROXY/HTTP_PROXY/NO_PROXY when no explicit
	// proxy is pinned.
	useEnvProxy bool
	// insecureSkipVerify disables certificate verification (STRICT_SSL opt-out).
	insecureSkipVerify bool
}

// dialTLSContext implements http.Transport.DialTLSContext. It performs the
// full dial itself — TCP (+ optional CONNECT tunnel) + uTLS handshake —
// because Transport-level proxying would finish the handshake with
// crypto/tls, defeating the fingerprint spoof.
func (d *utlsDialer) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("proxy.utlsDialer: invalid addr %q: %w", addr, err)
	}

	conn, err := d.dialTunnel(ctx, network, addr, host)
	if err != nil {
		return nil, err
	}

	uconn := utls.UClient(conn, &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: d.insecureSkipVerify,
	}, utls.HelloCustom)
	spec, err := http1OnlySpec(defaultClientHelloID())
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy.utlsDialer: fingerprint spec: %w", err)
	}
	if err := uconn.ApplyPreset(spec); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy.utlsDialer: apply fingerprint: %w", err)
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy.utlsDialer: uTLS handshake with %s: %w", host, err)
	}
	return uconn, nil
}

// dialTunnel opens a TCP connection to addr, tunneling through a proxy first
// when one is configured for the target host.
func (d *utlsDialer) dialTunnel(ctx context.Context, network, addr, host string) (net.Conn, error) {
	proxyURL := d.resolveProxy(host)
	dialer := NewSafeDialer()
	if proxyURL == "" {
		return dialer.DialContext(ctx, network, addr)
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("proxy.utlsDialer: invalid proxy URL: %w", err)
	}
	proxyAddr := canonicalProxyAddr(pu)
	conn, err := dialer.DialContext(ctx, network, proxyAddr)
	if err != nil {
		return nil, err
	}
	if err := proxyConnect(ctx, conn, addr, pu); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// resolveProxy returns the proxy URL for targetHost, or "" for direct.
// An explicit pin wins; otherwise the environment is consulted with exactly
// the semantics of http.ProxyFromEnvironment.
func (d *utlsDialer) resolveProxy(targetHost string) string {
	if d.explicitProxyURL != "" {
		return d.explicitProxyURL
	}
	if !d.useEnvProxy {
		return ""
	}
	// http.ProxyFromEnvironment only inspects req.URL, so a synthetic request
	// reproduces its HTTPS_PROXY/NO_PROXY behavior exactly.
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: targetHost}}
	if u, err := http.ProxyFromEnvironment(req); err == nil && u != nil {
		return u.String()
	}
	return ""
}

// canonicalProxyAddr returns host:port for a proxy URL, defaulting the port.
func canonicalProxyAddr(pu *url.URL) string {
	host := pu.Hostname()
	port := pu.Port()
	if port == "" {
		if pu.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

// proxyConnect performs an HTTP CONNECT handshake for targetAddr over an
// already-dialed proxy connection.
func proxyConnect(ctx context.Context, conn net.Conn, targetAddr string, proxyURL *url.URL) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("proxy.utlsDialer: set CONNECT deadline: %w", err)
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	var req strings.Builder
	req.WriteString("CONNECT " + targetAddr + " HTTP/1.1\r\n")
	req.WriteString("Host: " + targetAddr + "\r\n")
	if ui := proxyURL.User; ui != nil {
		pw, _ := ui.Password()
		creds := ui.Username() + ":" + pw
		req.WriteString("Proxy-Authorization: Basic " +
			base64.StdEncoding.EncodeToString([]byte(creds)) + "\r\n")
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		return fmt.Errorf("proxy.utlsDialer: CONNECT write: %w", err)
	}

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("proxy.utlsDialer: CONNECT read: %w", err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("proxy.utlsDialer: CONNECT headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	parts := strings.SplitN(status, " ", 3)
	if len(parts) < 2 || parts[1] != "200" {
		return fmt.Errorf("proxy.utlsDialer: CONNECT %s refused: %s", targetAddr, strings.TrimSpace(status))
	}
	// A proxy sends nothing after the CONNECT response until the client speaks
	// (TLS ClientHello), so the bufio reader cannot have swallowed TLS bytes.
	return nil
}

// defaultClientHelloID selects the spoofed fingerprint. Chrome is the default:
// it is the most common fingerprint on the wire and the least likely to be
// singled out. Override with UTLS_FINGERPRINT=firefox|safari|randomized.
func defaultClientHelloID() utls.ClientHelloID {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("UTLS_FINGERPRINT"))) {
	case "firefox":
		return utls.HelloFirefox_Auto
	case "safari":
		return utls.HelloSafari_Auto
	case "randomized":
		return utls.HelloRandomized
	default:
		return utls.HelloChrome_Auto
	}
}

// http1OnlySpec returns the ClientHelloSpec for base with ALPN pinned to
// http/1.1 only.
//
// The parrot fingerprints advertise "h2" in ALPN, and servers happily
// negotiate it — but our http.Transport drives the uTLS conn as HTTP/1.1
// (it cannot do HTTP/2 over a non-*tls.Conn), so a negotiated h2 would
// corrupt the stream. Pinning ALPN keeps the Chrome cipher/extension
// fingerprint intact while forcing h1, which every upstream API speaks.
func http1OnlySpec(base utls.ClientHelloID) (*utls.ClientHelloSpec, error) {
	spec, err := utls.UTLSIdToSpec(base)
	if err != nil {
		return nil, fmt.Errorf("UTLSIdToSpec: %w", err)
	}
	exts := make([]utls.TLSExtension, len(spec.Extensions))
	for i, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			cp := *alpn
			cp.AlpnProtocols = []string{"http/1.1"}
			exts[i] = &cp
		} else {
			exts[i] = ext
		}
	}
	spec.Extensions = exts
	return &spec, nil
}
