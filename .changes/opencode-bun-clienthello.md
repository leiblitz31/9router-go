### fix(opencode): present Bun's ClientHello on the free lane's DoH transport

- The free lane dialed opencode.ai through the shared DoH transport, which
  performs its uTLS handshake with Chrome's cipher list. The free-tier gate
  compares the JA4 cipher-list hash and answers 429 FreeUsageLimitError for
  any list that drops an entry, so Chrome's list was refused while Bun's
  17-suite list (Node ce060c7) is accepted.
- The free lane (`opencode`, alias `oc`) now gets its own cached DoH
  transport whose uTLS handshake carries Bun's exact cipher list; Chrome's
  extensions, curves and versions are kept, and ALPN stays pinned to
  http/1.1 (the Transport cannot drive HTTP/2 over a non-*tls.Conn).
- Scoped strictly to the free lane: zen/go lanes keep their existing
  transports (their direct path already dials through the Bun stealth
  stack), and every other provider keeps Chrome — the variant transport is
  cached under its own key and never shared. Same pool, same DoH
  resolution, same CONNECT proxy behaviour, same SSRF safe dialer; only the
  ClientHello cipher list changes.
- Verified: new unit tests for the Bun spec (exact order, freshness),
  the dialer spec selection (factory wins, ALPN pinned, input unmutated)
  and the free-lane selector; executor suite green, proxy and chat suites
  green except pre-existing failures proven identical at HEAD
  (TestRequestThroughALoopbackProxyReachesIt and the two strict-proxy leak
  tests — not touched, not weakened).
