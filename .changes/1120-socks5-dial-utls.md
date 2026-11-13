### SOCKS5 dial in the uTLS dialer (upstream #1120)

- SOCKS5 proxy pools now actually dial: the uTLS dialer performs the
  SOCKS5 handshake itself (RFC 1928, RFC 1929 username/password auth) via
  `golang.org/x/net/proxy` for `socks5://` and `socks5h://` pool URLs,
  instead of sending an HTTP CONNECT a real SOCKS5 endpoint refuses.
  `socks5://` resolves the target host locally, `socks5h://` hands the
  hostname to the proxy for remote resolution; the default SOCKS port is
  now 1080.
- The fingerprint survives: `t.Proxy` stays nil and the TLS handshake
  still runs through `DialTLSContext` (uTLS), proven end-to-end by a test
  that counts `DialTLSContext` invocations while a request travels through
  a SOCKS5 server to a local TLS upstream
  (`TestDialTLSContextViaSocks5`). A live in-test SOCKS5 server also pins
  the handshake itself — address type, auth, refusal, and data flow
  (`TestDialTunnelSocks5`).
- The HTTP CONNECT path is unchanged; the scheme dispatch lives in the
  dialer's per-target attempt loop, so DoH failover and attempt timeouts
  apply to SOCKS attempts exactly as they do to CONNECT attempts.
- Limits: TCP CONNECT only — UDP ASSOCIATE is not implemented (the
  gateway only needs CONNECT), and GSSAPI auth is not offered.
- Verified: `go build ./...`, `go vet ./...` green; `go test
  ./internal/proxy/...` plus the `handlers/chat` and `handlers/dashboard`
  packages green; full `go test ./...` shows only the pre-existing
  sandbox artefacts (DNS-dependent SSRF/hostname tests in `media`/`tunnel`,
  one timing-budget flake in `clientconfig`).
