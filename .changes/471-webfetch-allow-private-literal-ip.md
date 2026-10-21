### fix(media): let `ALLOW_PRIVATE_NETWORKING` reach the literal-IP branch too

Upstream luqman-v1/9router-go#471 (commit d135145). Upstream, `targetPrivate`
answered `ALLOW_PRIVATE_NETWORKING` on the name branch only: a literal IP was
judged directly by `proxy.IsPrivateIP` before the switch was consulted, so
`http://127.0.0.1:8080/` was refused even with the operator's flag switched on,
while `http://localhost/` — the same address by name — was forwarded. The fix
answers the flag first, before either branch, so a literal IP and a name that
resolves to it get the same verdict.

Port note for this fork: no code change was required, and none is taken.
`targetPrivate` and the caller-visible web-fetch pre-check it lives in were
introduced upstream by #459, which this fork has not taken — local
`HandleWebFetch` forwards without a request-scoped pre-check, so the only SSRF
judge here is the dialer's socket-level guard in `internal/proxy/ssrf.go`
(`newSafeDialerForProxy`). That guard already answers
`IsPrivateNetworkingAllowed()` first, before it parses or judges the literal
dial address, so the inconsistent verdict #471 fixes cannot occur on this
fork: with the flag on, literal IPs and resolved names are both allowed; with
it off, both are judged by the same `IsPrivateIP` check after resolution.

Verified by inspection of the ControlContext ordering in
`internal/proxy/ssrf.go` (the `IsPrivateNetworkingAllowed()` early return
precedes the `net.ParseIP` / `IsPrivateIP` branch), and by the absence of any
other request-scoped `IsPrivateIP` call site in the tree. If #459's pre-check
is ever ported, it must keep this ordering: the operator switch first, both
address shapes second.
