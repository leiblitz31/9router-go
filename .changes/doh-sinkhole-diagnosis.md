### DoH: name a sinkholed answer, and name where a TLS retry actually went

- A DoH pool whose resolver sinkholes a name (cloudflare and nextdns answer
  `0.0.0.1` for domains under a block order) used to report
  `proxy.dohResolver: no public A records in answer`, which reads like a broken
  resolver and sends the operator hunting in the DNS client instead of the
  resolver's policy. `parseDoHResponse` now names the reserved addresses it
  dropped: `resolver answered only reserved addresses (0.0.0.1): the name is
  sinkholed or blocked at this resolver`. The answer was never dialed — the
  reserved-address guard already refused it — so this is diagnosis, not a
  behaviour change on the wire. Verified live: a `cloudflare`-only pool answers
  `502 ... reserved addresses (0.0.0.1)`, the same provider answers 200 on a
  `google` + adguard pool.
- The insecure-TLS replay (the fallback for middleboxes that re-sign TLS) now
  carries provenance: `dohResolver` records the address and endpoint behind the
  last resolution per host, and both the warning and the propagated error say
  `resolved to <ip> via <endpoint>`. A certificate valid for no name at all is
  the signature of a dial that landed somewhere other than the hostname asked
  for, and the hostname alone could not tell that apart from a re-signing proxy.
  Pinned by `TestDoHResolutionNoteNamesAddressAndResolver`.
- `TestProxyIdentityIsUnchanged` called `tr.Proxy(req)` on a transport built
  with an explicit proxy, whose `Proxy` field is deliberately nil (the uTLS
  dialer tunnels manually) — it panicked, taking the whole `internal/proxy`
  package down with it. It asserts through `proxyURLFor` now, the read the data
  plane itself performs.
