### fix(proxy): exempt the operator's own outbound proxy from the SSRF guard

Every upstream call on an install whose connections egress through a proxy on
loopback was refused:

```
upstream error: forward to upstream: forward to https://<provider>/v1/chat/completions:
assigned proxy failed, refusing to route direct (Post "...": proxyconnect tcp:
dial tcp 127.0.0.1:8888: request to private/reserved IP blocked by SSRF protection):
proxy required but failed (strictProxy=true)
```

The proxy does not have to come from the environment: a connection configured in
the dashboard with `connectionProxyEnabled` and a `connectionProxyUrl` of
`http://127.0.0.1:8888` — a local CONNECT proxy for DNS, a sandbox sidecar, the
standard Termux setup — hits it too. On the reporting install that is **68 of 69
connections**, so the failure reads as "many broken providers" rather than "one
broken proxy". The environment-variable path (`HTTP_PROXY=http://127.0.0.1:8888`)
breaks identically.

`NewSafeDialer`'s `ControlContext` runs on every dial the transport makes,
including the dial to the proxy. A request routed through a proxy never dials the
target itself, so the guard was inspecting the wrong host: it protected nothing
about the destination and instead removed the operator's egress path. Because
`connectionProxyEnabled` also marks the request strict, the direct replay is
forbidden, so the request has no way out at all — a 502 that names SSRF for a
healthy local proxy.

`internal/proxy/ssrf.go` now carries a bounded `proxyAllowlist` of dial endpoints
exempt from the private-IP check:

- `NewSafeTransportForProxy(proxyURL)` installs the proxy on the transport and
  exempts its endpoint; an empty or unparseable URL falls back to the ambient
  path so a malformed pool URL cannot silently become a direct dial.
- `NewSafeTransport()` keeps `t.Proxy` **nil** and exempts whatever endpoint the
  environment names, resolved through `proxyFromEnvironment`. Leaving the field
  nil is deliberate: `proxyFuncIsAssigned` (`proxy_egress.go`) tells an assigned
  proxy from an ambient one by comparing func identity against
  `http.ProxyFromEnvironment`, so wrapping `t.Proxy` to observe the host would
  have made every ambient proxy read as assigned — and with it every strict
  request would refuse the direct replay the ambient case exists to allow.
- The three callers that built a transport and then assigned `transport.Proxy` by
  hand now go through the constructor: `InsecureClientFor`, the per-connection
  proxy client cache in `internal/handlers/chat/connections_proxy.go` (the path
  this report exercised), and the dashboard's proxy test in
  `internal/handlers/dashboard/settings.go`.

The guard itself is unchanged for targets: loopback, private, link-local,
unspecified and cloud-metadata addresses are still refused unless the request
carries `WithAllowLoopback` (local-by-design providers) or
`ALLOW_PRIVATE_NETWORKING` is set.

**Verified.** A/B on the real gateway stack — same request, same combo, same
upstream, same database, only the binary differing, with no `NO_PROXY` and the
connection proxy on `127.0.0.1:8888`: the pre-fix binary answered `502` in 0.076s
with the message above; the fixed binary answered `200` in 2.0s with the model's
completion, and its usage log confirms the request left through the proxy
(`egress=http://127.0.0.1:8888` on the 200) rather than quietly falling back
direct. A separate run that pointed `HTTP_PROXY` at a dead port left the chat path
unaffected — the connection's own proxy is what drives it — while the catalog sync
that does use the environment proxy failed with a plain `connection refused`,
which is the behaviour the exemption is meant to leave alone.
`internal/proxy/ssrf_proxy_allowlist_test.go` covers the exemption end to end: a
real listener on `127.0.0.1` acting as the proxy is reached through
`http.Client.Get`, and the transport's own `DialContext` connects to that listener
while an unrelated loopback listener is still refused — plus the ambient path,
the bare-host entry, a bounded allowlist, every previously blocked private target,
and the proxy-identity invariant above.

**Related.** The same install's healthy production instance only worked because it
ran with `ALLOW_PRIVATE_NETWORKING=true`, which short-circuits the guard before any
of this — the blunt workaround for the same defect. This fix lets that variable be
dropped and the guard turned back on.

**Documented limitation.** A proxy named by *hostname* is not matched, because Go
resolves the name before `ControlContext` sees the address, and comparing there
would need a DNS lookup in the dial path. Every local proxy — the case this fixes
— is written as a literal address, and a hostname proxy that resolves to a public
IP was never blocked in the first place.