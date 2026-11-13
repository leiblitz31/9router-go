### SOCKS5 proxy pools: adapt tests to the fork's uTLS transport (upstream #1120)

- Upstream #1120 pins SOCKS5 pool support by asserting `t.Proxy` is set.
  The fork keeps `t.Proxy` nil by design: the uTLS dialer tunnels through
  the assigned proxy itself, because net/http would otherwise finish a
  proxied handshake with crypto/tls and kill the JA4 fingerprint the
  OpenCode free lane depends on. The three tests now assert the fork's
  equivalent contract instead: `t.Proxy` stays nil, `DialTLSContext` is
  present (the fingerprinted handshake path), and `ProxyURLFor` reports the
  assigned proxy from the transport registry
  (`TestNewSafeTransportHonoursAmbientEnv`,
  `TestGetClientForConnection_Socks5ProxyPool`,
  `TestProbeHTTPClient_Socks5Proxy`).
- Two production seams, behaviour-preserving: the uTLS dialer's ambient
  resolution and the registry introspection now consult the package's
  `proxyFromEnvironment` indirection (which defaults to the stdlib
  function) instead of calling the stdlib directly, so the ambient-proxy
  behaviour is observable in tests without depending on the stdlib's
  once-cached environment view.
- `FallbackTransport` restores the #1120 behaviour the merge dropped: when
  the primary leg fails outright and the direct fallback is refused by the
  SSRF guard, the primary proxy error surfaces instead of the guard
  verdict (`TestFallbackTransportPrimaryErrorOnSSRF`).
- Verified: `go build ./...`, `go vet ./...` green; `go test ./...` 68
  packages OK, remaining failures are pre-existing sandbox artefacts
  (DNS-dependent SSRF/hostname tests in `media`/`tunnel`, one timing-budget
  flake in `clientconfig` that passes standalone).
