package petaniproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// petaniUserAgent mirrors the header petani-proxy sends on its own requests.
const petaniUserAgent = "Mozilla/5.0"

func normalizedProtocol(proto string) string {
	p := strings.ToLower(strings.TrimSpace(proto))
	if p == "" {
		return ProtocolHTTP
	}
	return p
}

func joinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// splitProxyAddr splits an "ip:port" string, tolerating a missing port.
func splitProxyAddr(addr string) (host string, port int) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.Trim(addr, "[]"), 0
	}
	port, _ = strconv.Atoi(p)
	return h, port
}

// transportForProxy builds an *http.Transport that routes requests through
// the given proxy. http/https use Go's standard proxy support (CONNECT for
// https targets); socks5 uses golang.org/x/net/proxy; socks4/socks4a use the
// hand-rolled dialer below (curl_cffi, which checker.py prefers, speaks
// SOCKS4a for hostname targets, so we do too).
func transportForProxy(proto, proxyAddr string, timeout time.Duration) (*http.Transport, error) {
	tr := &http.Transport{
		// Explicit proxy config: never consult the environment here, so a
		// test proxy on 127.0.0.1 is used even when HTTP(S)_PROXY is set.
		Proxy:                 nil,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	switch normalizedProtocol(proto) {
	case ProtocolHTTP, ProtocolHTTPS:
		u, err := url.Parse(normalizedProtocol(proto) + "://" + proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("petaniproxy: bad proxy address %q: %w", proxyAddr, err)
		}
		tr.Proxy = http.ProxyURL(u)
	case ProtocolSocks5:
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			d, err := proxy.SOCKS5("tcp", proxyAddr, nil, ctxDialer{ctx: ctx, timeout: timeout})
			if err != nil {
				return nil, fmt.Errorf("petaniproxy: socks5 dialer: %w", err)
			}
			return d.Dial(network, addr)
		}
	case ProtocolSocks4:
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return socks4Dial(ctx, proxyAddr, addr, timeout)
		}
	default:
		return nil, fmt.Errorf("petaniproxy: unsupported protocol %q", proto)
	}
	return tr, nil
}

// ctxDialer adapts net.Dialer to proxy.Dialer while honoring ctx.
type ctxDialer struct {
	ctx     context.Context
	timeout time.Duration
}

func (d ctxDialer) Dial(network, address string) (net.Conn, error) {
	return (&net.Dialer{Timeout: d.timeout}).DialContext(d.ctx, network, address)
}

// socks4Dial performs a SOCKS4/4a CONNECT handshake to targetAddr through
// the SOCKS4 proxy at proxyAddr. Hostname targets use the SOCKS4a extension
// (DSTIP 0.0.0.x + hostname after USERID), mirroring curl_cffi behavior.
func socks4Dial(ctx context.Context, proxyAddr, targetAddr string, timeout time.Duration) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: socks4 bad target %q: %w", targetAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("petaniproxy: socks4 bad target port %q", portStr)
	}

	var dstIP [4]byte
	var domainSuffix []byte
	if ip4 := net.ParseIP(host).To4(); ip4 != nil {
		copy(dstIP[:], ip4)
	} else {
		// SOCKS4a: unresolved hostname goes after the empty USERID.
		dstIP = [4]byte{0, 0, 0, 1}
		domainSuffix = append([]byte(host), 0x00)
	}

	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: socks4 connect to proxy: %w", err)
	}
	// Bound the handshake itself; cleared on success below.
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return nil, err
	}
	req := []byte{0x04, 0x01, byte(port >> 8), byte(port), dstIP[0], dstIP[1], dstIP[2], dstIP[3], 0x00}
	req = append(req, domainSuffix...)
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("petaniproxy: socks4 handshake write: %w", err)
	}
	resp := make([]byte, 8)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("petaniproxy: socks4 handshake read: %w", err)
	}
	if resp[0] != 0x00 || resp[1] != 0x5A {
		conn.Close()
		return nil, fmt.Errorf("petaniproxy: socks4 request rejected (code %d)", resp[1])
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
