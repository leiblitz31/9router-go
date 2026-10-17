// Package petaniproxy is a Go port of the petani-proxy harvester core
// (core/checker.py and core/fast_validator.py): concurrent proxy
// connectivity checking, latency measurement, anonymity classification,
// fast latency validation, and sync of live proxies into the 9Router
// SQLite proxy pool.
//
// Concurrency model: goroutines bounded by errgroup.SetLimit (worker pool
// with a configurable concurrency cap) instead of Python's
// ThreadPoolExecutor / asyncio.Semaphore. Every public function takes a
// context.Context and aborts in-flight work on cancellation (early-stop
// when the alive target is reached).
//
// Network-dependent lookups (host IP detection, ip-api.com GeoIP) use
// package-level endpoint variables so tests can redirect them at local
// httptest servers; no test in this package touches the public internet.
package petaniproxy

import "time"

// Anonymity levels reported for a live proxy, matching checker.py.
const (
	AnonymityTransparent = "Transparent"
	AnonymityAnonymous   = "Anonymous"
	AnonymityElite       = "Elite"
)

// Supported proxy protocols.
const (
	ProtocolHTTP   = "http"
	ProtocolHTTPS  = "https"
	ProtocolSocks4 = "socks4"
	ProtocolSocks5 = "socks5"
)

// ProxyCandidate is a single proxy to test. It mirrors the dicts produced
// by petani-proxy's harvesters: {"ip", "port", "proxy", "protocol"} where
// "proxy" is the "ip:port" string and "protocol" is one of
// http/https/socks4/socks5.
type ProxyCandidate struct {
	IP       string
	Port     int
	Proxy    string // "ip:port"
	Protocol string // http, https, socks4, socks5
}

// Address returns the "ip:port" dial address, falling back to IP+Port
// when Proxy is empty.
func (c ProxyCandidate) Address() string {
	if c.Proxy != "" {
		return c.Proxy
	}
	return joinHostPort(c.IP, c.Port)
}

// URL returns the "proto://ip:port" URL form stored by fast_validator.py
// as the proxy's "url" field.
func (c ProxyCandidate) URL() string {
	return normalizedProtocol(c.Protocol) + "://" + c.Address()
}

// GeoInfo carries GeoIP/ISP enrichment for a live proxy.
type GeoInfo struct {
	Country     string
	CountryCode string
	City        string
	ISP         string
}

// UnknownGeo is the default GeoInfo used before enrichment, matching
// checker.py's "Unknown"/"??"/"-"/"-" placeholders.
func UnknownGeo() GeoInfo {
	return GeoInfo{Country: "Unknown", CountryCode: "??", City: "-", ISP: "-"}
}

// LiveProxy is a proxy that passed validation, enriched with latency,
// egress IP, anonymity level and geo data. It merges the result dicts of
// checker.py::test_single_proxy and fast_validator.py::check_*_proxy.
type LiveProxy struct {
	IP          string
	Port        int
	Proxy       string // "ip:port"
	Protocol    string
	URL         string // "proto://ip:port"
	LatencySec  float64
	LatencyMs   int
	EgressIP    string
	Anonymity   string // Transparent | Anonymous | Elite ("" when not classified)
	Country     string
	CountryCode string
	City        string
	ISP         string
	TestedAt    time.Time
}

// Geo returns the proxy's GeoInfo view.
func (p *LiveProxy) Geo() GeoInfo {
	return GeoInfo{
		Country:     p.Country,
		CountryCode: p.CountryCode,
		City:        p.City,
		ISP:         p.ISP,
	}
}

// SetGeo applies enrichment, keeping existing values for empty fields.
func (p *LiveProxy) SetGeo(g GeoInfo) {
	if g.Country != "" {
		p.Country = g.Country
	}
	if g.CountryCode != "" {
		p.CountryCode = g.CountryCode
	}
	if g.City != "" {
		p.City = g.City
	}
	if g.ISP != "" {
		p.ISP = g.ISP
	}
}
