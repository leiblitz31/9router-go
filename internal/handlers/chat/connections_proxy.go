package chat

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"9router/proxy/internal/log"
	internalproxy "9router/proxy/internal/proxy"
	"9router/proxy/internal/proxy/executor"
)

// maxProxyClients caps rotating-proxy growth: each entry pins a Transport
// plus idle sockets, so unbounded distinct URLs (residential rotation)
// would leak descriptors without eviction.
const maxProxyClients = 128

// strictCachePrefix namespaces the InsecureStrict-free, no-direct-replay
// variants in proxyClients: a strict pool and a lax one may point at the same
// URL, and sharing a client would leak the strict decision onto whichever
// connection asked second.
const strictCachePrefix = "strict\x00"

var (
	proxyClientsMu sync.RWMutex
	proxyClients   = make(map[string]*http.Client)
	// proxyPoolLogged tracks pool IDs already announced at debug level so the
	// "routing via proxy" line appears once per pool, not on every request.
	proxyPoolLogged sync.Map
)

// logProxyOnce records proxy usage once per pool at debug level, e.g.
// proxy routing requests via proxy pool pool=abc123 url=http://... type=http
// Run with LOG_LEVEL=debug and grep for "proxy" to confirm traffic uses the pool.
func logProxyOnce(poolID, proxyURLStr, proxyType string) {
	if poolID == "" {
		return
	}
	if _, loaded := proxyPoolLogged.LoadOrStore(poolID, true); loaded {
		return
	}
	if proxyType == "" {
		proxyType = "http"
	}
	log.Debug("proxy", "routing requests via proxy pool", "pool", poolID, "url", proxyURLStr, "type", proxyType)
}

// GetClientForConnection returns an http.Client configured with ProxyPool transport if set.
// A connection bound to a pool that cannot serve traffic gets an error instead of
// a client: silently dropping the pool would send the request out from the real IP,
// which is the one thing the assignment exists to prevent.
// Provider is unknown here (media callers resolve no provider), so the
// provider-scoped transport variants are selected by passing "" — the
// chat path passes the resolved provider instead.
func (h *ChatHandler) GetClientForConnection(connData *ConnectionData) (*http.Client, error) {
	return h.getClientForConnection(connData, "")
}

// isOpencodeFreeLane reports whether provider is the OpenCode free lane
// (canonical "opencode"; the "oc" alias resolves to it). Only the free lane
// gets the Bun ClientHello on its DoH transport: the gate compares the JA4
// cipher-list hash, and Chrome's list answers 429 while Bun's answers 200.
// Zen/go lanes keep their existing transports (their direct path already
// dials through the Bun stealth stack), and every other provider keeps
// Chrome — the variant transport is cached under its own key, never shared.
func isOpencodeFreeLane(provider string) bool {
	return resolveProviderAlias(provider) == "opencode"
}

func (h *ChatHandler) getClientForConnection(connData *ConnectionData, provider string) (*http.Client, error) {
	if connData == nil {
		return h.Client, nil
	}

	var proxyURLStr string
	var proxyType string
	// dohSpecs holds every resolver spec of a bound "doh" pool. The pool
	// block below fills it; the doh branch further down builds ONE transport
	// from the whole list so resolution can fail over between resolvers
	// instead of depending on the single spec NextURL happened to pick.
	var dohSpecs []string
	// The row's own flag, which the pool below may override. Seeding it here is
	// what makes a legacy connection's strict setting mean anything: the
	// declaration alone would leave it false for every connection that is not
	// bound to a pool.
	strictProxy := connData.StrictProxy
	// Whether the operator meant this connection to leave through a proxy at
	// all. Upstream gates its strictProxy refusal on exactly this
	// (decolua/9router#4333): executors such as Qoder set strictProxy to mean
	// "do not replay this request directly if the proxy fails" — a replayed
	// COSY signature answers 403 — not "a proxy must exist". With nothing
	// configured those callers must keep working direct, so a strict flag
	// alone may never turn into an error.
	proxyIntended := connData.ProxyPoolID != ""

	// 1. Resolve from ProxyPool. A pool that cannot serve traffic (deleted,
	// inactive, or carrying no URL) makes the request fail: silently going
	// direct would publish the operator's egress IP, which is the one thing a
	// proxy was assigned to prevent, and the dashboard would keep showing the
	// connection as proxied.
	if connData.ProxyPoolID != "" {
		pool, err := h.Repo.GetProxyPool(connData.ProxyPoolID)
		switch {
		case err != nil || pool == nil:
			log.Warn("proxy", "assigned pool not found", "pool", connData.ProxyPoolID)
			return nil, fmt.Errorf("proxy pool %q is assigned but does not exist", connData.ProxyPoolID)
		case !pool.IsActive:
			log.Warn("proxy", "assigned pool is inactive", "pool", connData.ProxyPoolID)
			return nil, fmt.Errorf("proxy pool %q is assigned but inactive", connData.ProxyPoolID)
		}
		proxyURLStr = pool.NextURL()
		if proxyURLStr == "" {
			log.Warn("proxy", "assigned pool has no url", "pool", connData.ProxyPoolID)
			return nil, fmt.Errorf("proxy pool %q is assigned but carries no url", connData.ProxyPoolID)
		}
		proxyType = pool.Type
		strictProxy = pool.StrictProxy
		if pool.Type == "doh" {
			dohSpecs = pool.URLs
		}
		// Name the pool for the log line. The lookup already happened, so this
		// costs nothing, and "Vercel Relay" answers a question a UUID cannot.
		connData.ResolvedProxyPool = pool.Name

		if pool.IsEdgeRelay() {
			// A relay is an HTTPS endpoint that forwards on x-relay-target /
			// x-relay-path, not a CONNECT proxy. Dialing it with http.ProxyURL
			// answers "malformed HTTP status code" (see db.ProxyPool.IsEdgeRelay).
			// getProviderConfig rewrites the base URL to the relay and injects
			// those headers, so the plain client is the correct transport here —
			// the request still leaves through the relay, just not as a tunnel.
			logProxyOnce(connData.ProxyPoolID, proxyURLStr, proxyType)
			return h.Client, nil
		}
	}

	// 2. Fallback to legacy connection proxy
	if proxyURLStr == "" {
		proxyEnabled := connData.ConnectionProxyEnabled
		proxyURL := connData.ConnectionProxyURL
		if connData.ProviderSpecificData != nil {
			if en, ok := connData.ProviderSpecificData["connectionProxyEnabled"].(bool); ok {
				proxyEnabled = en
			}
			if u, ok := connData.ProviderSpecificData["connectionProxyUrl"].(string); ok {
				proxyURL = u
			}
			if sp, ok := connData.ProviderSpecificData["strictProxy"].(bool); ok {
				strictProxy = sp
			}
		}
		if proxyEnabled || strings.TrimSpace(proxyURL) != "" {
			proxyIntended = true
		}
		if proxyEnabled && proxyURL != "" {
			proxyURLStr = proxyURL
			proxyType = "http"
		}
	}

	if proxyURLStr == "" {
		if strictProxy && proxyIntended {
			// Strict mode means "never leave over the direct IP". A connection
			// that names a proxy, has one enabled, or is bound to a pool, but
			// resolves no usable URL, is exactly that case: an empty or
			// discarded URL would otherwise go straight out from the real
			// address while the dashboard shows it proxied (#4333).
			log.Error("proxy", "strict proxy enabled but none resolved", "pool", connData.ProxyPoolID, "proxyUrl", connData.ConnectionProxyURL)
			return nil, fmt.Errorf("%w: the connection enables a proxy but resolves none", internalproxy.ErrStrictProxyRequired)
		}
		return h.Client, nil
	}
	logProxyOnce(connData.ProxyPoolID, proxyURLStr, proxyType)

	if proxyType == "doh" {
		// Pool type "doh": the pool's URLs are resolver specs (doh-mcp
		// aliases such as "cloudflare"/"google", or DoH JSON endpoint URLs),
		// not proxy addresses — they must never reach url.Parse below. One
		// transport takes the whole list and fails resolution over between
		// resolvers (see proxy.NewSafeTransportWithDoH); the cache key names
		// the pool, not the single spec NextURL picked for proxyURLStr.
		specs := dohSpecs
		if len(specs) == 0 {
			specs = []string{proxyURLStr}
		}
		// The OpenCode free lane presents Bun's ClientHello on this
		// transport (see isOpencodeFreeLane): the gate compares the JA4
		// cipher-list hash, and Chrome's list is refused. The variant is
		// cached under its own key so no other provider ever shares it.
		bunHello := isOpencodeFreeLane(provider)
		cacheKey := "doh\x00" + connData.ProxyPoolID + "\x00" + strings.Join(specs, "\x00")
		if bunHello {
			cacheKey = "doh\x00bun\x00" + connData.ProxyPoolID + "\x00" + strings.Join(specs, "\x00")
		}
		if strictProxy {
			cacheKey = strictCachePrefix + cacheKey
		}
		return h.cachedProxyClient(cacheKey, strictProxy, func() *http.Transport {
			if bunHello {
				return internalproxy.NewSafeTransportWithDoHSpec(specs, executor.OpencodeBunSpecFactory())
			}
			return internalproxy.NewSafeTransportWithDoH(specs...)
		}), nil
	}

	parsedURL, err := url.Parse(proxyURLStr)
	if err == nil && (parsedURL.Scheme == "" || parsedURL.Host == "") {
		err = fmt.Errorf("no scheme or host in %q", proxyURLStr)
	}
	if err != nil {
		log.Warn("proxy", "invalid proxy pool url", "pool", connData.ProxyPoolID, "url", proxyURLStr, "error", err)
		// The operator named a proxy and the transport cannot dial it. Under a
		// strict connection that is a refusal, not a reason to leave directly.
		if connData.ProxyPoolID != "" {
			// The connection is bound to this pool, so a URL the transport
			// cannot parse is a broken assignment, not a missing one.
			return nil, fmt.Errorf("proxy pool %q has an unparseable url", connData.ProxyPoolID)
		}
		if strictProxy {
			log.Error("proxy", "strict proxy enabled but proxy url invalid", "url", proxyURLStr)
			return nil, fmt.Errorf("%w: unparseable proxy url %q", internalproxy.ErrStrictProxyRequired, proxyURLStr)
		}
		return h.Client, nil
	}

	if proxyType == "http" || proxyType == "" {
		// The OpenCode free lane presents Bun's ClientHello on this
		// transport too (see isOpencodeFreeLane): through an HTTP proxy
		// the default Chrome list is refused by the free-tier JA4 gate,
		// exactly as on the DoH transport. The variant is cached under
		// its own key so no other provider ever shares it.
		if isOpencodeFreeLane(provider) {
			bunKey := "http\x00bun\x00" + proxyURLStr
			if strictProxy {
				bunKey = strictCachePrefix + bunKey
			}
			return h.cachedProxyClient(bunKey, strictProxy, func() *http.Transport {
				return internalproxy.NewSafeTransportWithProxySpec(parsedURL.String(), false, executor.OpencodeBunSpecFactory())
			}), nil
		}
		cacheKey := proxyURLStr
		if strictProxy {
			cacheKey = strictCachePrefix + proxyURLStr
		}
		return h.cachedProxyClient(cacheKey, strictProxy, func() *http.Transport {
			return internalproxy.NewSafeTransportWithProxy(parsedURL.String(), false)
		}), nil
	}

	// For Edge Relays (vercel, cloudflare, deno), standard client is used because
	// URL rewriting and x-relay headers are handled at request time.
	return h.Client, nil
}

// cachedProxyClient returns the shared client for cacheKey, building its
// transport on first use. The cache is shared by every proxied branch (http
// pools, doh pools) so eviction and the strict marker behave identically.
func (h *ChatHandler) cachedProxyClient(cacheKey string, strictProxy bool, build func() *http.Transport) *http.Client {
	proxyClientsMu.RLock()
	client, ok := proxyClients[cacheKey]
	proxyClientsMu.RUnlock()
	if ok {
		return client
	}

	proxyClientsMu.Lock()
	defer proxyClientsMu.Unlock()
	if client, ok = proxyClients[cacheKey]; ok {
		return client
	}
	// Evict idle sockets of a random victim when over cap (amortized O(1);
	// exact LRU is overkill — URLs are hot or dead, never warm).
	if len(proxyClients) >= maxProxyClients {
		for victimURL, victim := range proxyClients {
			victim.CloseIdleConnections()
			delete(proxyClients, victimURL)
			break
		}
	}

	client = &http.Client{
		Transport: build(),
		Timeout:   h.Client.Timeout,
	}
	if strictProxy {
		// The marker travels with the client, so every caller that dials a
		// strict pool — the chat forwarder, the media lanes, the executors —
		// refuses the direct replay in proxy.DoRequest without having to
		// thread a flag through each of them.
		client = internalproxy.ForbidDirectReplay(client)
	}
	proxyClients[cacheKey] = client
	return client
}
