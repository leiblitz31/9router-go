### Custom loopback nodes are local by design for the SSRF guard

- Dispatch to a custom provider node whose declared `baseUrl` is loopback
  (`127.0.0.0/8`, `::1`, or `localhost`) now passes the SSRF guard, the same
  way a registry provider flagged `LocalByDesign` does. Previously only the
  static registry flag opened loopback, so every custom node on loopback —
  e.g. a single-user sidecar relay on `127.0.0.1:20232` — failed dispatch
  with 502 "request to private/reserved IP blocked by SSRF protection" even
  though the operator registered that exact endpoint.
- The allowance is loopback-only and fail-closed: a private LAN address, a
  Tailscale address, a public host, a missing node row, and a malformed or
  scheme-less URL all stay blocked. No provider is rerouted and no alias is
  created; the node a request already resolves to is the only one examined.
- Verified: new table-driven unit tests for the loopback classifier plus the
  nil-handler/nil-repo guards, and a live probe of a loopback custom node
  that previously failed at the guard now reaches its backend.
