package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DNS-over-HTTPS resolution for "doh" proxy pools (see doh_transport.go).
//
// A doh pool's URLs are resolver specs, not proxy addresses: an alias shared
// with doh-mcp ("cloudflare", "google", "quad9", "adguard") or a DoH JSON
// endpoint URL. The transport resolves the target host through the chosen
// resolver and dials the resolved IP, so a poisoned or blocked system
// resolver never gets a say in where upstream traffic goes.

// dohAliases maps resolver specs to their DoH JSON endpoints. The mapping is
// byte-identical to doh-mcp's, so a spec means the same resolver in both
// tools. A spec that is itself an https URL is used verbatim.
var dohAliases = map[string]string{
	"cloudflare": "https://cloudflare-dns.com/dns-query",
	"google":     "https://dns.google/resolve",
	"quad9":      "https://dns.quad9.net/dns-query",
	"adguard":    "https://dns.adguard-dns.com/dns-query",
}

// dohEndpoint resolves a resolver spec to its DoH JSON endpoint URL.
func dohEndpoint(spec string) string {
	spec = strings.TrimSpace(spec)
	if endpoint, ok := dohAliases[strings.ToLower(spec)]; ok {
		return endpoint
	}
	return spec
}

// DoHEndpoint maps a resolver spec to the DoH JSON endpoint it queries: a
// known alias resolves through dohAliases, anything else is used verbatim.
// The dashboard renders this mapping next to each spec so an operator can
// see which service a spec names without the frontend duplicating the alias
// table and drifting from the data plane's own reading of it.
func DoHEndpoint(spec string) string {
	return dohEndpoint(spec)
}

// DoHAliases returns the alias → endpoint table, copied so a caller cannot
// mutate the data plane's own map.
func DoHAliases() map[string]string {
	out := make(map[string]string, len(dohAliases))
	for alias, endpoint := range dohAliases {
		out[alias] = endpoint
	}
	return out
}

const (
	// dohCacheMaxEntries bounds the resolve cache: a gateway dials a small
	// set of provider hosts, so 512 live names is generous headroom, and the
	// bound keeps a hostile caller from growing the map name by name.
	dohCacheMaxEntries = 512
	dohCacheMinTTL     = 30 * time.Second
	dohCacheMaxTTL     = 600 * time.Second
	dohQueryTimeout    = 10 * time.Second
	// dohEndpointTimeout bounds one resolver attempt inside a multi-resolver
	// failover: a stalled endpoint must not eat the whole query budget when
	// the pool names two more resolvers that would answer in a second.
	dohEndpointTimeout = 6 * time.Second
	dohMaxAnswerBytes  = 1 << 20
)

// dohCacheEntry is one host's resolved A records and their expiry.
type dohCacheEntry struct {
	ips     []string
	expires time.Time
	next    uint64 // rotation cursor for pickIP
}

// dohResolver resolves hostnames through one or more DoH endpoints, with a
// bounded TTL cache. Safe for concurrent use.
//
// Multiple endpoints come from a multi-URL doh pool: a query starts at a
// rotating endpoint and fails over to the next one on error or timeout, so
// one stalled resolver cannot take the pool down with it — the failure mode
// a strict connection would otherwise surface as a hard 502.
type dohResolver struct {
	endpoints []string
	client    *http.Client
	mu        sync.Mutex
	cache     map[string]*dohCacheEntry
	rotate    atomic.Uint64
}

// newDoHResolver builds a resolver for one or more specs (aliases or
// endpoint URLs). No spec at all falls back to cloudflare.
//
// The query client honors the proxy environment via http.ProxyFromEnvironment:
// this machine has no direct egress, so the DoH query itself must leave
// through the ambient proxy — the same one the data path tunnels through.
func newDoHResolver(specs ...string) *dohResolver {
	endpoints := make([]string, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		ep := dohEndpoint(spec)
		if ep == "" {
			continue
		}
		if _, dup := seen[ep]; dup {
			continue
		}
		seen[ep] = struct{}{}
		endpoints = append(endpoints, ep)
	}
	if len(endpoints) == 0 {
		endpoints = []string{dohAliases["cloudflare"]}
	}
	return &dohResolver{
		endpoints: endpoints,
		client: &http.Client{
			Timeout: dohQueryTimeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				DialContext:         NewSafeDialer().DialContext,
				TLSHandshakeTimeout: dohQueryTimeout,
			},
		},
		cache: make(map[string]*dohCacheEntry),
	}
}

// resolveDialTarget maps addr ("host:port") to "ip:port" using DoH. A literal
// IP host passes through untouched.
func (r *dohResolver) resolveDialTarget(ctx context.Context, host, addr string) (string, error) {
	if net.ParseIP(host) != nil {
		return addr, nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("proxy.dohResolver: invalid addr %q: %w", addr, err)
	}
	ip, err := r.pickIP(ctx, host)
	if err != nil {
		return "", err
	}
	rememberDoHResolution(host, ip, "")
	return net.JoinHostPort(ip, port), nil
}

// dohResolution records where a host was last sent: the address that was
// dialed and the resolver that produced it. Without it a TLS failure on a DoH
// path reports only the hostname, so "certificate is not valid for any names"
// says nothing about whether the name was resolved correctly — the layer the
// operator has to check first.
type dohResolution struct {
	ip       string
	resolver string
}

const dohResolutionMaxHosts = 1024

var (
	dohResolutionMu sync.RWMutex
	dohResolutions  = make(map[string]dohResolution)
)

// rememberDoHResolution fills in whichever half of host's last resolution the
// caller knows: the resolver before the dial rotates a pick, the address at
// dial time.
func rememberDoHResolution(host, ip, resolver string) {
	if host == "" {
		return
	}
	dohResolutionMu.Lock()
	defer dohResolutionMu.Unlock()
	if len(dohResolutions) >= dohResolutionMaxHosts {
		// ponytail: bounded map, drop everything rather than evict; this is
		// diagnostic state only. Swap for an LRU if it ever needs history.
		dohResolutions = make(map[string]dohResolution)
	}
	entry := dohResolutions[host]
	if ip != "" {
		entry.ip = ip
	}
	if resolver != "" {
		entry.resolver = resolver
	}
	dohResolutions[host] = entry
}

// DoHResolutionFor reports the last DoH address and resolver this process
// used for host, so an error raised after the dial can name them. ok is false
// when host was never resolved through DoH (a literal IP, or a non-DoH path).
func DoHResolutionFor(host string) (ip, resolver string, ok bool) {
	dohResolutionMu.RLock()
	defer dohResolutionMu.RUnlock()
	entry, found := dohResolutions[host]
	if !found || (entry.ip == "" && entry.resolver == "") {
		return "", "", false
	}
	return entry.ip, entry.resolver, true
}

// ProbeDoHResolvers resolves hostname through the given resolver specs and
// returns the round-trip latency of the successful answer. It backs the
// dashboard's pool test for doh pools: the pool passes when at least one of
// its resolvers answers, which is precisely the failover pickIP performs
// across the resolver's endpoints, so the probe wraps it instead of
// duplicating the DNS query path.
func ProbeDoHResolvers(ctx context.Context, specs []string, hostname string) (int64, error) {
	return probeDoHResolvers(ctx, newDoHResolver(specs...), hostname)
}

// ProbeDoHResolver probes ONE resolver spec on its own — no failover to a
// sibling endpoint — and returns the round-trip latency of its answer. The
// dashboard's per-resolver test uses it to grade every spec in a doh pool
// individually, which the aggregate ProbeDoHResolvers cannot do: a pool
// passes there when any single resolver answers, so a dead spec would stay
// invisible behind its healthy siblings.
func ProbeDoHResolver(ctx context.Context, spec, hostname string) (int64, error) {
	return probeDoHResolvers(ctx, newDoHResolver(spec), hostname)
}

func probeDoHResolvers(ctx context.Context, resolver *dohResolver, hostname string) (int64, error) {
	start := time.Now()
	if _, err := resolver.pickIP(ctx, hostname); err != nil {
		return 0, fmt.Errorf("proxy.ProbeDoHResolvers: %w", err)
	}
	return time.Since(start).Milliseconds(), nil
}

// pickIP returns one public A record for host, rotating across the cached
// records so a multi-homed target spreads load the way round-robin DNS would.
func (r *dohResolver) pickIP(ctx context.Context, host string) (string, error) {
	ips, err := r.resolveA(ctx, host)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	var idx uint64
	if e := r.cache[host]; e != nil {
		idx = e.next
		e.next++
	}
	r.mu.Unlock()
	return ips[idx%uint64(len(ips))], nil
}

// resolveA returns host's public A records, from cache when fresh.
func (r *dohResolver) resolveA(ctx context.Context, host string) ([]string, error) {
	if ips, ok := r.cached(host); ok {
		return ips, nil
	}
	ips, ttl, err := r.query(ctx, host)
	if err != nil {
		return nil, err
	}
	r.store(host, ips, ttl)
	return ips, nil
}

func (r *dohResolver) cached(host string) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[host]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return append([]string(nil), e.ips...), true
}

func (r *dohResolver) store(host string, ips []string, ttl time.Duration) {
	if ttl < dohCacheMinTTL {
		ttl = dohCacheMinTTL
	}
	if ttl > dohCacheMaxTTL {
		ttl = dohCacheMaxTTL
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= dohCacheMaxEntries {
		now := time.Now()
		for k, e := range r.cache {
			if now.After(e.expires) {
				delete(r.cache, k)
			}
		}
		if len(r.cache) >= dohCacheMaxEntries {
			// Still full of live entries: skip caching this name rather
			// than evict a hot one. The caller already has its answer.
			return
		}
	}
	r.cache[host] = &dohCacheEntry{
		ips:     append([]string(nil), ips...),
		expires: time.Now().Add(ttl),
	}
}

// dohResponse is the subset of a dns-json answer this resolver consumes.
type dohResponse struct {
	Answer []struct {
		Data string `json:"data"`
		TTL  int    `json:"TTL"`
	} `json:"Answer"`
}

// parseDoHResponse decodes a dns-json answer body and returns the public A
// records it carries plus the smallest TTL among them. Private, loopback or
// otherwise reserved answers are dropped: a DoH answer pointing homewards is
// a rebinding attempt, not a resolution (see IsPrivateIP).
//
// A sinkholed or legally blocked name answers with nothing but reserved
// addresses (cloudflare and nextdns return 0.0.0.1 for domains under a block
// order), so the error names those addresses: "no public A records" alone
// reads like a broken resolver and sends the operator hunting in the wrong
// layer. The answer is never dialed either way.
func parseDoHResponse(body []byte) ([]string, time.Duration, error) {
	var resp dohResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, 0, fmt.Errorf("proxy.dohResolver: decode dns-json: %w", err)
	}
	var ips []string
	var reserved []string
	ttl := dohCacheMaxTTL
	for _, a := range resp.Answer {
		ip := net.ParseIP(a.Data)
		if ip == nil || ip.To4() == nil {
			continue // CNAME targets and AAAA records are not A dial targets.
		}
		if IsPrivateIP(ip) {
			reserved = append(reserved, ip.String())
			continue
		}
		ips = append(ips, ip.String())
		if a.TTL > 0 && time.Duration(a.TTL)*time.Second < ttl {
			ttl = time.Duration(a.TTL) * time.Second
		}
	}
	if len(ips) == 0 {
		if len(reserved) > 0 {
			return nil, 0, fmt.Errorf("proxy.dohResolver: resolver answered only reserved addresses (%s): the name is sinkholed or blocked at this resolver",
				strings.Join(reserved, ", "))
		}
		return nil, 0, fmt.Errorf("proxy.dohResolver: no public A records in answer")
	}
	return ips, ttl, nil
}

// query asks the resolver's endpoints for host's A records, starting at a
// rotating offset and failing over to the next endpoint on any error. The
// first successful answer wins; when every endpoint fails, the last error
// is returned.
func (r *dohResolver) query(ctx context.Context, host string) ([]string, time.Duration, error) {
	start := r.rotate.Add(1) - 1
	var lastErr error
	for i := range r.endpoints {
		endpoint := r.endpoints[(start+uint64(i))%uint64(len(r.endpoints))]
		attemptCtx, cancel := context.WithTimeout(ctx, dohEndpointTimeout)
		ips, ttl, err := r.queryEndpoint(attemptCtx, endpoint, host)
		cancel()
		if err == nil {
			return ips, ttl, nil
		}
		lastErr = err
	}
	return nil, 0, lastErr
}

// queryEndpoint asks one DoH endpoint for host's A records.
func (r *dohResolver) queryEndpoint(ctx context.Context, endpoint, host string) ([]string, time.Duration, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, 0, fmt.Errorf("proxy.dohResolver: invalid endpoint %q: %w", endpoint, err)
	}
	q := u.Query()
	q.Set("name", host)
	q.Set("type", "A")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("proxy.dohResolver: build query: %w", err)
	}
	req.Header.Set("Accept", "application/dns-json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("proxy.dohResolver: query %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("proxy.dohResolver: %s answered %s", endpoint, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, dohMaxAnswerBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("proxy.dohResolver: read answer: %w", err)
	}
	ips, ttl, err := parseDoHResponse(body)
	if err != nil {
		return nil, 0, err
	}
	rememberDoHResolution(host, "", endpoint)
	return ips, ttl, nil
}
