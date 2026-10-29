### OpenCode free lane keeps Bun's ClientHello on the direct path

- Upstream decolua/9router#5310 principle (TLS fingerprint must ride every
  path, not just the proxied ones): `getClientForConnection` now returns a
  dedicated Bun-hello client for the OpenCode free lane when no pool and no
  proxy is configured, instead of the plain (Chrome-hello) client. The DoH
  path already presented Bun's list (2ec1e311) and the HTTP-proxy path got
  it in a741dcaf (decolua/9router#5297); the direct path was the last one
  sending Chrome's list, which the free-tier JA4 gate refuses.
- The client is cached under its own `direct\x00bun\x00` key so it never
  shares a transport with the Chrome plain client or the pooled variants.
  Other providers are untouched (they still get the plain client), and the
  strict marker is deliberately not applied here: a direct dial is the path
  itself, not a fallback to forbid.
- Deliberate divergence from decolua/9router#5298/#5310: the native
  client-mcp service bridge (local `opencode serve` daemon per proxy URL,
  WebSocket `/api/experimental/client-mcp`, session/turn management, and the
  dashboard Local Service Daemon toggle) is NOT ported — this stack has no
  daemon and talks directly to the opencode.ai HTTP API, so porting the
  bridge would mean building a fake daemon, not a port. The #5310
  behaviors that do map here already exist in translated form: TLS
  fingerprint behind a proxy (a741dcaf), fallback when the primary path
  fails (pool rotation on free-tier refusal, FallbackTransport, 8d66cddf),
  and session lifecycle/resilience (stable session per connection with
  rotation on budget spent). The four catalog models added by #5310 were
  not added: none of them appears in the live /v1/models catalog, so
  adding them would pollute it with unverified entries.
- Verified by a new behavioral test that captures the ClientHello on a
  local listener: the `opencode` lane and the `oc` alias send Bun's 17
  ciphers in order on the direct path, while any other provider keeps the
  plain client. The test fails on the pre-change code (negative control).
  Full chat package: only the 2 known pre-existing strict-proxy failures;
  full proxy package: only the known loopback failure; go vet and gofmt
  clean.
