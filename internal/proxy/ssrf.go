package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"9router/proxy/internal/constants"
)

var (
	// ErrSSRFBlocked is returned when an outbound dial attempts to reach a private,
	// loopback, link-local, or cloud metadata IP address without authorization.
	ErrSSRFBlocked = errors.New("request to private/reserved IP blocked by SSRF protection")
)

type allowLoopbackKey struct{}

// WithAllowLoopback returns a child context that permits connections to loopback
// addresses (127.0.0.1, ::1, localhost). Used for local-by-design providers
// like ollama-local.
func WithAllowLoopback(ctx context.Context) context.Context {
	return context.WithValue(ctx, allowLoopbackKey{}, true)
}

// IsLoopbackAllowed reports whether loopback connections are permitted in the context.
func IsLoopbackAllowed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(allowLoopbackKey{}).(bool)
	return v
}

// IsPrivateIP reports whether an IP address is private, loopback, link-local,
// cloud metadata (169.254.x.x), or reserved.
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.IsLoopback() || ip4.IsPrivate() || ip4.IsLinkLocalUnicast() ||
			ip4.IsLinkLocalMulticast() || ip4.IsUnspecified() || ip4.IsMulticast() ||
			(ip4[0] == 169 && ip4[1] == 254) || ip4[0] == 0
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// IsPrivateNetworkingAllowed reports whether connections to private/local IPs
// are globally permitted via ALLOW_PRIVATE_NETWORKING=true/1.
func IsPrivateNetworkingAllowed() bool {
	v := strings.TrimSpace(os.Getenv("ALLOW_PRIVATE_NETWORKING"))
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	return false
}

// NewSafeDialer returns a *net.Dialer equipped with an active socket ControlContext
// hook that aborts connection attempts to private or reserved IP addresses after
// DNS resolution (mitigating DNS rebinding).
func NewSafeDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		ControlContext: func(ctx context.Context, network, address string, c syscall.RawConn) error {
			if IsPrivateNetworkingAllowed() {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				host = address
			}
			ip := net.ParseIP(host)
			if ip != nil {
				if ip.IsLoopback() && IsLoopbackAllowed(ctx) {
					return nil
				}
				if IsPrivateIP(ip) {
					return ErrSSRFBlocked
				}
			}
			return nil
		},
	}
}


// NewSafeTransport returns an *http.Transport configured with production pool
// defaults, an active SSRF-filtering DialContext, and a uTLS-spoofed TLS
// handshake (see utls_dialer.go).
//
// The transport honors the standard proxy environment variables
// (HTTPS_PROXY, HTTP_PROXY, NO_PROXY) with exactly the semantics of
// http.ProxyFromEnvironment, matching the behavior of the Node.js 9Router.
// This is required for relay connections (Vercel/Cloudflare edge pools): the
// relay URL itself must be reached through the egress proxy, and without env
// proxy support the TLS handshake fails with
// "tls: first record does not look like a TLS handshake".
//
// NOTE: t.Proxy is intentionally left nil. The uTLS dialer tunnels through
// the proxy itself, because net/http would otherwise finish a proxied
// handshake with crypto/tls (Transport.dialConn/addTLS), defeating the
// fingerprint spoof. Use proxiesViaAssignment / proxyURLFor (not t.Proxy)
// to introspect how a transport dials.
//
// When no proxy env vars are set, the dialer connects directly.
func newTransportWithUTLS(d *utlsDialer, kind transportProxyKind, url string) *http.Transport {
	t := constants.DefaultHTTPTransportConfig.NewTransport()
	t.DialContext = NewSafeDialer().DialContext
	t.DialTLSContext = d.dialTLSContext
	registerTransportProxy(t, kind, url)
	return t
}

func NewSafeTransport() *http.Transport {
	return newTransportWithUTLS(&utlsDialer{useEnvProxy: true}, proxyKindEnv, "")
}

// NewSafeTransportDirect returns an *http.Transport identical to
// NewSafeTransport but always dialing directly (no proxy), with the same
// uTLS-spoofed handshake. It is the "true direct" transport used by
// FallbackTransport when the environment proxy refuses a connection:
// retrying through the same broken env proxy would be pointless, so the
// fallback must bypass it entirely.
func NewSafeTransportDirect() *http.Transport {
	return newTransportWithUTLS(&utlsDialer{}, proxyKindNone, "")
}

// NewSafeTransportWithProxy returns a transport identical to NewSafeTransport
// but pinned to an explicit proxy URL (e.g. a per-connection
// connectionProxyUrl) instead of the environment. An empty proxyURL means
// direct. insecure disables certificate verification, mirroring
// InsecureSkipVerify.
func NewSafeTransportWithProxy(proxyURL string, insecure bool) *http.Transport {
	if proxyURL == "" {
		return newTransportWithUTLS(&utlsDialer{insecureSkipVerify: insecure}, proxyKindNone, "")
	}
	return newTransportWithUTLS(
		&utlsDialer{explicitProxyURL: proxyURL, insecureSkipVerify: insecure},
		proxyKindExplicit, proxyURL,
	)
}
