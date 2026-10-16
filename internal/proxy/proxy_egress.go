package proxy

import (
	"net/http"
	"net/url"
	"reflect"
)

// proxiesViaAssignment reports whether the caller's client routes through a proxy
// the operator deliberately chose, rather than one picked up from the ambient
// environment.
//
// The distinction decides whether a failed attempt may be re-sent direct. A proxy
// inherited from HTTP_PROXY — or a local sandbox the operator never asked for —
// refusing the tunnel is noise to route around, and going direct is the intended
// escape. A proxy pool assigned to the connection is the operator's stated egress
// path: re-dialing direct answers the request from the host's real IP, which is
// exactly what the assignment exists to prevent, so the failure must surface.
//
// Transports built by NewSafeTransport* record their egress choice in the
// registry (see utls_dialer.go) because t.Proxy stays nil there — the uTLS
// dialer tunnels manually so net/http cannot finish the handshake with
// crypto/tls. Transports with t.Proxy assigned directly (legacy/tests) fall
// back to the func-pointer comparison.
func proxiesViaAssignment(client *http.Client) bool {
	if client == nil {
		return false
	}
	return transportProxyIsAssigned(client.Transport)
}

func transportProxyIsAssigned(rt http.RoundTripper) bool {
	switch t := rt.(type) {
	case nil:
		return false
	case *http.Transport:
		if lookupTransportProxy(t).kind == proxyKindExplicit {
			return true
		}
		return proxyFuncIsAssigned(t.Proxy)
	case *FallbackTransport:
		// FallbackTransport owns a Base/Direct pair and already escapes an
		// ambient proxy through its own Direct leg. Base is the path the caller
		// chose, so that is the one to inspect.
		return transportProxyIsAssigned(t.Base)
	default:
		return false
	}
}

func proxyFuncIsAssigned(proxy func(*http.Request) (*url.URL, error)) bool {
	if proxy == nil {
		return false
	}
	ambient := reflect.ValueOf(http.ProxyFromEnvironment).Pointer()
	return reflect.ValueOf(proxy).Pointer() != ambient
}
