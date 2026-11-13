package proxy

import (
	"net/http"
	"testing"
)

// TestNewSafeTransportHonoursAmbientEnv pins the fix for the silent ambient
// bypass: a *http.Transport with Proxy nil never consults the environment, so
// NewSafeTransport must honour HTTP_PROXY / ALL_PROXY (including the values
// syncEnvFromViper lifts from .env) even though it never installs t.Proxy.
//
// Fork adaptation of upstream #1120: t.Proxy stays nil by design — the uTLS
// dialer tunnels through the proxy itself, because net/http would otherwise
// finish a proxied handshake with crypto/tls (Transport.dialConn/addTLS),
// defeating the fingerprint spoof. The ambient proxy is therefore honoured
// by the dialer (resolveProxy, through the proxyFromEnvironment indirection)
// and reported by the transport registry (ProxyURLFor), never through
// t.Proxy. The test asserts that chain, not the upstream pin.
func TestNewSafeTransportHonoursAmbientEnv(t *testing.T) {
	// Swapped through the package's proxyFromEnvironment indirection: calling
	// the stdlib function directly would depend on whether an earlier test in
	// the binary already froze its once-cache with a different environment.
	withProxyResolver(t, "socks5h://192.168.22.206:1080")
	tr := NewSafeTransport()
	if tr.Proxy != nil {
		t.Fatal("NewSafeTransport installed t.Proxy; the uTLS dialer tunnels manually and must keep it nil")
	}
	if tr.DialTLSContext == nil {
		t.Fatal("NewSafeTransport has no DialTLSContext; the fingerprinted handshake path is missing")
	}
	if got := lookupTransportProxy(tr); got.kind != proxyKindEnv {
		t.Fatalf("transport proxy kind = %v, want proxyKindEnv (ambient proxy tunneling)", got)
	}
	// The dialer itself must resolve the ambient proxy for the target host.
	d := &utlsDialer{useEnvProxy: true}
	if got := d.resolveProxy("api.opencode.ai"); got != "socks5h://192.168.22.206:1080" {
		t.Fatalf("utlsDialer ambient proxy = %q, want socks5h://192.168.22.206:1080", got)
	}
	// ... and the registry must report the same proxy for the transport.
	if got := ProxyURLFor(&http.Client{Transport: tr}); got != "socks5h://192.168.22.206:1080" {
		t.Fatalf("ProxyURLFor(NewSafeTransport) = %q, want socks5h://192.168.22.206:1080", got)
	}
}

// TestNewDirectSafeTransportNeverProxies pins the escape legs: the direct
// transport behind FallbackTransport's fallback and replayDirect must dial
// from the host's own address even when the environment names a proxy.
func TestNewDirectSafeTransportNeverProxies(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "socks5h://192.168.22.206:1080")
	t.Setenv("HTTP_PROXY", "socks5h://192.168.22.206:1080")
	tr := NewDirectSafeTransport()
	if tr.Proxy != nil {
		t.Fatal("NewDirectSafeTransport installed a Proxy func; the escape leg must stay proxy-free")
	}
}

// TestDefaultDirectIsLazy pins the init-order constraint: resolving the direct
// leg at package-init time would fire http.ProxyFromEnvironment's once-cache
// before main() ran viper's .env sync, freezing an empty ambient view for the
// whole process.
func TestDefaultDirectIsLazy(t *testing.T) {
	if defaultDirectTransport != nil {
		t.Fatal("defaultDirectTransport resolved at package init; it must stay nil until defaultDirect() runs post-boot")
	}
	rt := defaultDirect()
	if rt == nil {
		t.Fatal("defaultDirect() returned nil")
	}
}
