### Embedded DoH proxy pools; Petani Proxy removed

- **New proxy pool type `doh`.** A pool with `type: "doh"` stores resolver
  specs in its `urls` — a doh-mcp alias (`cloudflare`, `google`, `quad9`,
  `adguard`, mapped to the same DoH JSON endpoints doh-mcp uses) or a DoH
  endpoint URL — and `NextURL()` rotates across them like any other pool.
  `NewSafeTransportWithDoH` resolves the target host over DNS-over-HTTPS
  (TTL cache, bounded at 512 entries, private/reserved answers rejected as
  rebinding attempts) and dials the resolved public IP; when the environment
  installs a proxy, the CONNECT tunnel names the resolved IP so the proxy
  cannot re-resolve through the bypassed system resolver. One transport
  takes the pool's whole resolver list: a query starts at a rotating
  resolver, each attempt is capped at 6s, and failure fails over to the
  next resolver — a single stalled resolver (observed live: one.one.one.one
  timing out) must not fail strict-pinned requests the others would answer.
  This replaces the external doh-proxy adapter process that previously sat
  in front of the pool. The resolution is spliced into the uTLS dialer's existing tunnel
  dial, so the spoofed ClientHello, ALPN pinning and SSRF socket guard are
  unchanged — no separate crypto/tls path was introduced.
- **Petani Proxy removed.** The embedded petani-proxy subsystem is deleted
  end to end: `internal/petaniproxy/`, `internal/proxygateway/`, the
  `/api/proxy-gateway/*` dashboard handlers and routes, the `__petani__`
  special case in chat connection proxy resolution, and the dashboard tab,
  nav entry, routes and API client methods. Connections still carrying the
  `__petani__` pool id now fail as an assigned-but-missing pool instead of
  silently dialing a fixed loopback gateway.
- Verified with table-driven unit tests (no external network): DoH alias
  mapping, dns-json parsing against an `httptest` fake resolver, rejection
  of private-IP answers, resolve-cache behaviour, and `doh` pool parsing /
  rotation / `IsEdgeRelay` in `internal/db`; `go build ./...` and
  `cd web && bun run build` pass.
