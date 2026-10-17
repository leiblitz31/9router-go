package petaniproxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Shared test helpers for checker_test.go and validator_test.go.
// Everything runs against local httptest servers; no external network.

// startIPServer serves {"ip": <ip>} JSON like api.ipify.org, optionally
// with extra response headers.
func startIPServer(t *testing.T, ip string, extraHeaders map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range extraHeaders {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ip":%q}`, ip)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// startForwardProxy runs a minimal HTTP forward proxy (absolute-URI
// requests are relayed directly, bypassing any environment proxy).
func startForwardProxy(t *testing.T) string {
	t.Helper()
	direct := &http.Transport{Proxy: nil}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			http.Error(w, "forward proxy needs absolute URI", http.StatusBadRequest)
			return
		}
		out, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := direct.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host // "127.0.0.1:port"
}

// relayConn bridges a proxy client connection to its target.
func relayConn(client, target net.Conn) {
	defer client.Close()
	defer target.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(target, client) }()
	go func() { defer wg.Done(); io.Copy(client, target) }()
	wg.Wait()
}

// startSocks5Server runs a minimal no-auth SOCKS5 CONNECT server.
func startSocks5Server(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks5: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSocks5Conn(c)
		}
	}()
	return ln.Addr().String()
}

func handleSocks5Conn(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, int(head[1]))); err != nil {
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil { // no auth
		return
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return
	}
	var host string
	switch hdr[3] {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	p := make([]byte, 2)
	if _, err := io.ReadFull(c, p); err != nil {
		return
	}
	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(p[0])<<8|int(p[1]))), 5*time.Second)
	if err != nil {
		c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	c.SetDeadline(time.Time{})
	relayConn(c, target)
}

// startSocks4Server runs a minimal SOCKS4/4a CONNECT server.
func startSocks4Server(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks4: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSocks4Conn(c)
		}
	}()
	return ln.Addr().String()
}

func handleSocks4Conn(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return
	}
	is4a := hdr[4] == 0 && hdr[5] == 0 && hdr[6] == 0 && hdr[7] != 0
	var domain []byte
	for {
		var b [1]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return
		}
		if b[0] == 0x00 {
			break
		}
		if is4a {
			domain = append(domain, b[0])
		}
	}
	var host string
	if is4a {
		host = string(domain)
	} else {
		host = net.IP(hdr[4:8]).String()
	}
	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(hdr[2])<<8|int(hdr[3]))), 5*time.Second)
	if err != nil {
		c.Write([]byte{0x00, 0x5B, 0, 0, 0, 0, 0, 0})
		return
	}
	c.Write([]byte{0x00, 0x5A, 0, 0, 0, 0, 0, 0})
	c.SetDeadline(time.Time{})
	relayConn(c, target)
}

// overrideHostIPEndpoints swaps the host-IP endpoints for a test.
func overrideHostIPEndpoints(t *testing.T, urls ...string) {
	t.Helper()
	old := hostIPEndpoints
	hostIPEndpoints = urls
	t.Cleanup(func() { hostIPEndpoints = old })
	ResetHostIPCache()
	t.Cleanup(ResetHostIPCache)
}

// overrideGeoIPBatchURL swaps the batch GeoIP endpoint for a test.
func overrideGeoIPBatchURL(t *testing.T, url string) {
	t.Helper()
	old := geoIPBatchURL
	geoIPBatchURL = url
	t.Cleanup(func() { geoIPBatchURL = old })
}

// overrideGeoIPSingleURLFmt swaps the single GeoIP endpoint format for a test.
func overrideGeoIPSingleURLFmt(t *testing.T, format string) {
	t.Helper()
	old := geoIPSingleURLFmt
	geoIPSingleURLFmt = format
	t.Cleanup(func() { geoIPSingleURLFmt = old })
}

// testCandidate builds a ProxyCandidate from a "host:port" dial address.
func testCandidate(addr, proto string) ProxyCandidate {
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	return ProxyCandidate{IP: host, Port: port, Proxy: addr, Protocol: proto}
}
