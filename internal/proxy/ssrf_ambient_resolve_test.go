package proxy

import (
	"net"
	"net/http"
	"net/url"
	"testing"
)

// The sandbox egress proxy is named by hostname in the environment and
// resolves into the RFC2544 benchmark range, which the netguard predicate
// treats as reserved. The ambient allowlist must therefore remember the
// resolved addresses, not just the name as written — otherwise the guard
// refuses every dial to the operator's own proxy.
func TestProxyAllowlistHostsResolvesHostname(t *testing.T) {
	u, err := url.Parse("http://localhost:3128")
	if err != nil {
		t.Fatal(err)
	}
	hosts := proxyAllowlistHosts(u)
	found := map[string]bool{}
	for _, h := range hosts {
		found[h] = true
	}
	if !found["localhost:3128"] {
		t.Errorf("literal host missing from %v", hosts)
	}
	if !found["127.0.0.1"] && !found["127.0.0.1:3128"] {
		t.Errorf("resolved loopback form missing from %v", hosts)
	}
}

// A literal-IP proxy needs no resolution and passes through unchanged.
func TestProxyAllowlistHostsLiteralIP(t *testing.T) {
	u, _ := url.Parse("http://198.19.0.1:3128")
	hosts := proxyAllowlistHosts(u)
	if len(hosts) != 1 || hosts[0] != "198.19.0.1:3128" {
		t.Errorf("literal proxy expanded unexpectedly: %v", hosts)
	}
}

// End to end: an ambient proxy named by hostname, listening on loopback,
// must be dialable through NewSafeDialer even though loopback is guarded.
func TestAmbientProxyNamedByHostnameIsDialable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	orig := proxyFromEnvironment
	proxyFromEnvironment = func(req *http.Request) (*url.URL, error) {
		return url.Parse("http://localhost:" + port)
	}
	defer func() { proxyFromEnvironment = orig }()

	conn, err := NewSafeDialer().DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial ambient proxy by resolved address: %v", err)
	}
	conn.Close()
}
