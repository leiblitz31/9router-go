package proxy

import (
	"testing"
)

// TestNewSafeTransportHonoursAmbientEnv pins the fix for the silent ambient
// bypass: HTTP_PROXY / ALL_PROXY (including the values syncEnvFromViper
// lifts from .env) must decide the egress of a poolless connection. The
// fork's mechanism differs from upstream's: the uTLS dialer resolves the
// ambient proxy itself through the proxyFromEnvironment indirection, so
// t.Proxy stays nil by design and the transport is registered as env-kind
// instead. The assertions below pin that fork seam to the same outcome.
func TestNewSafeTransportHonoursAmbientEnv(t *testing.T) {
	// Swapped through the package's proxyFromEnvironment indirection: calling
	// the stdlib function directly would depend on whether an earlier test in
	// the binary already froze its once-cache with a different environment.
	withProxyResolver(t, "socks5h://192.168.22.206:1080")
	tr := NewSafeTransport()
	if a := lookupTransportProxy(tr); a.kind != proxyKindEnv {
		t.Fatalf("NewSafeTransport registry kind = %v, want the env (ambient) kind", a.kind)
	}
	u := ambientProxyURL()
	if u == nil || u.String() != "socks5h://192.168.22.206:1080" {
		t.Fatalf("ambient proxy = %v, want socks5h://192.168.22.206:1080", u)
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
