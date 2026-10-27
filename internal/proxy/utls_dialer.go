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

// dohDialAttemptTimeout bounds one address attempt when a DoH answer carries
// more than one record. A healthy CONNECT through the ambient proxy measured
// 0.4–1.9s here, so this leaves generous headroom while still handing the next
// record a turn instead of letting one stalled address consume the request.
const dohDialAttemptTimeout = 8 * time.Second

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
	// specFactory, when set, builds a fresh ClientHelloSpec per handshake
	// instead of the default Chrome (or UTLS_FINGERPRINT override) spec.
	// It must return a fresh spec on every call: the dialer pins ALPN on a
	// copy, but the spec's extension objects are otherwise shared with the
	// handshake. Nil keeps the historical behaviour.
	specFactory ClientHelloSpecFactory
	// doh, when non-nil, resolves the target host over DNS-over-HTTPS and
	// dials the resolved public IP instead of letting the system resolver
	// answer (see doh_resolve.go). Only the TCP destination changes: the
	// uTLS ServerName in dialTLSContext still comes from the original addr,
	// and a proxy CONNECT names the resolved IP so the proxy cannot
	// re-resolve the host through the bypassed resolver.
	doh *dohResolver
}

// ClientHelloSpecFactory builds one fresh ClientHelloSpec per handshake.
// Callers that return a shared spec risk concurrent handshakes racing on it.
type ClientHelloSpecFactory func() (*utls.ClientHelloSpec, error)

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
	spec, err := d.helloSpec()
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

// dialPlainContext implements http.Transport.DialContext for a DoH dialer:
// the same tunnel dialTLSContext uses — DoH resolution plus an optional
// proxy CONNECT — without the TLS handshake on top.
func (d *utlsDialer) dialPlainContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("proxy.utlsDialer: invalid addr %q: %w", addr, err)
	}
	return d.dialTunnel(ctx, network, addr, host)
}

// dialTunnel opens a TCP connection to addr, tunneling through a proxy first
// when one is configured for the target host.
//
// With DoH in play the resolver answers with every A record, and the dial walks
// them in rotation order until one accepts. A multi-homed provider routinely
// publishes addresses this network cannot reach — observed live: 5 of 8 kiro
// runtime records and 1 of 2 kilo records answered the proxy's CONNECT with 502
// — and dialing only the first pick made every request a coin flip that cost a
// full CONNECT timeout when it lost.
func (d *utlsDialer) dialTunnel(ctx context.Context, network, addr, host string) (net.Conn, error) {
	proxyURL := d.resolveProxy(host)
	dialer := NewSafeDialer()

	targets := []string{addr}
	if d.doh != nil {
		resolved, err := d.doh.resolveDialTargets(ctx, host, addr)
		if err != nil {
			return nil, err
		}
		targets = resolved
	}

	if proxyURL == "" {
		var lastErr error
		for _, target := range targets {
			conn, err := dialer.DialContext(ctx, network, target)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, lastErr
	}

	pu, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("proxy.utlsDialer: invalid proxy URL: %w", err)
	}
	proxyAddr := canonicalProxyAddr(pu)
	var lastErr error
	for _, target := range targets {
		// One dead record must not consume the caller's whole budget: with a
		// single target the caller's deadline stands, but when there are more
		// records to try each attempt is capped so the next one still gets a
		// turn. dohDialAttemptTimeout is generous next to a healthy CONNECT
		// (observed 0.4–1.9s) and short next to a request timeout.
		attemptCtx := ctx
		cancel := func() {}
		if len(targets) > 1 {
			attemptCtx, cancel = context.WithTimeout(ctx, dohDialAttemptTimeout)
		}
		conn, err := dialer.DialContext(attemptCtx, network, proxyAddr)
		if err != nil {
			cancel()
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if err := proxyConnect(attemptCtx, conn, target, pu); err != nil {
			cancel()
			_ = conn.Close()
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		cancel()
		// Name the address that actually accepted, not the one that was asked
		// for: after a failover those differ, and a later TLS error has to
		// report where the request really went.
		if dialHost, _, splitErr := net.SplitHostPort(target); splitErr == nil {
			rememberDoHResolution(host, dialHost, "")
		}
		return conn, nil
	}
	return nil, lastErr
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

// helloSpec returns the ClientHelloSpec for one handshake: the factory's
// fresh spec when one is configured, otherwise the default Chrome (or
// UTLS_FINGERPRINT override) spec. Either way ALPN is pinned to http/1.1
// (see pinALPNH1).
func (d *utlsDialer) helloSpec() (*utls.ClientHelloSpec, error) {
	if d.specFactory != nil {
		spec, err := d.specFactory()
		if err != nil {
			return nil, err
		}
		return pinALPNH1(*spec), nil
	}
	return http1OnlySpec(defaultClientHelloID())
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
	return pinALPNH1(spec), nil
}

// pinALPNH1 returns a copy of spec with ALPN pinned to http/1.1 only.
// The input spec is never mutated: only the copy's Extensions slice header
// changes, and the ALPN extension itself is copied before rewriting, so a
// factory that hands out a shared spec stays safe to reuse.
func pinALPNH1(spec utls.ClientHelloSpec) *utls.ClientHelloSpec {
	cp := spec
	exts := make([]utls.TLSExtension, len(spec.Extensions))
	for i, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpnCopy := *alpn
			alpnCopy.AlpnProtocols = []string{"http/1.1"}
			exts[i] = &alpnCopy
		} else {
			exts[i] = ext
		}
	}
	cp.Extensions = exts
	return &cp
}
