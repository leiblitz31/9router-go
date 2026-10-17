### fix(proxy-pools): never dial an edge relay as an HTTP CONNECT proxy

A proxy pool whose `type` was left at the default `"http"` while its URL pointed
at a self-hosted edge relay was dialed as a CONNECT proxy. The CONNECT attempt
sent a raw TLS handshake at the relay, and the HTTP client read the relay's
plain-text reply as a status line:

```
malformed HTTP status code "\x01@\x00\x01\x00\x00\x10\x00\x00\x04\x00…"
ForwardOpencode: forward to https://opencode.ai/inference/v1/chat/completions
```

The same install worked as soon as the pool was detached, which is the tell: the
relay is a forwarding HTTPS endpoint, not a tunnel, and `ProxyPool.IsEdgeRelay()`
decided which one it was from the `type` field alone. With `"http"` there it
returned `false`, so `getProviderConfig` also skipped the rewrite that points
`BaseURL` at the relay and adds `x-relay-target` / `x-relay-path` — the two
things that make a relay work. Every request through that pool failed at the
transport, before any provider saw it.

`internal/db/proxyPools.go`:

- `IsEdgeRelayURL(rawURL)` recognises the platform hosts that serve relays
  (`*.vercel.app`, `*.workers.dev`, `*.deno.dev`, `*.netlify.app`) and the
  `/.netlify/functions/relay` path.
- `IsEdgeRelay()` now also returns `true` when the declared type is `""` or
  `"http"` **and** any pool URL matches `IsEdgeRelayURL`. A relay URL created
  with the default type can no longer be routed into the proxy path. The
  explicit `vercel` / `cloudflare` / `deno` / `netlify` types are unchanged, and
  a real proxy — loopback, a `:8080` CONNECT proxy, `socks5` — is still a proxy.

`internal/handlers/chat/connections_proxy.go`:

- `getClientForConnection` returns the plain `h.Client` for an edge relay
  instead of wrapping it in `http.ProxyURL`. The request still leaves through
  the relay; it simply travels as a forwarded request carrying the
  `x-relay-target` / `x-relay-path` headers `getProviderConfig` injects, not as
  a CONNECT tunnel.

**Verified.** `go build ./...` and `go test ./internal/db/...
./internal/handlers/chat/...` pass. New tests:
`TestProxyPool_IsEdgeRelay` covers the inference per host and the negatives (a
real proxy on `:8080`, a loopback proxy, `socks5`, and a lookalike
`vercel.app.evil.com` host that must not match);
`TestGetClientForConnection_RelayPoolDeclaredAsHTTP` pins that a relay pool
yields the plain client; and
`TestRelayPoolAssignedToNoAuthProviderUsesRelayHeaders` exercises the whole path
— a relay pool assigned to the no-auth `opencode` provider resolves to a
connection whose plain client is used, with `BaseURL` rewritten to the relay and
`x-relay-target: https://opencode.ai` plus
`x-relay-path: /inference/v1/chat/completions` on the forwarded request.

**For operators hitting this today:** setting the pool's type to the matching
platform (`vercel`, `cloudflare`, `deno`, `netlify`) also avoids the failure.
The code fix makes the default `"http"` safe as well, because the old failure
mode was a cryptic transport error rather than a configuration message.
