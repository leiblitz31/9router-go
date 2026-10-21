package proxy

import (
	"net/http"

	"9router/proxy/internal/constants"
)

// NewSafeTransportWithDoH returns an *http.Transport identical to
// NewSafeTransport — same pool defaults, same uTLS-spoofed handshake, same
// environment-proxy tunneling — except that target hostnames are resolved
// over DNS-over-HTTPS (see doh_resolve.go) and the TCP connection is made to
// the resolved public IP:
//
//   - resolverSpecs are doh-mcp aliases ("cloudflare", "google", "quad9",
//     "adguard") or DoH JSON endpoint URLs — a "doh" proxy pool's URLs,
//     passed whole. Resolution starts at a rotating spec and fails over to
//     the next on error or timeout, so one stalled resolver cannot fail a
//     request the others would have answered.
//   - When the environment installs a proxy, the CONNECT tunnel names the
//     resolved IP, not the hostname, so the proxy never re-resolves the name
//     through the system resolver the DoH path exists to bypass.
//   - The TLS ServerName and certificate verification still use the original
//     hostname; only the dial target changes.
//   - DoH answers carrying private/reserved IPs are rejected in the
//     resolver, and the SSRF socket guard stays in force for every dial this
//     transport makes (the proxy endpoint and any direct fallback alike).
func NewSafeTransportWithDoH(resolverSpecs ...string) *http.Transport {
	d := &utlsDialer{useEnvProxy: true, doh: newDoHResolver(resolverSpecs...)}
	t := constants.DefaultHTTPTransportConfig.NewTransport()
	t.DialContext = d.dialPlainContext
	t.DialTLSContext = d.dialTLSContext
	registerTransportProxy(t, proxyKindEnv, "")
	return t
}
