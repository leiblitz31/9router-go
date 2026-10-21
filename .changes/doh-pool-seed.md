### Default DoH resolver pool seeded on first boot

- **Fresh installs get a ready-to-use DoH pool.** On startup the gateway
  now plants a `doh-resolvers` pool (type `doh`, round-robin, strictProxy,
  active) carrying the 15 verified resolver specs — the `cloudflare` and
  `google` aliases plus 13 DoH JSON endpoints (Cloudflare 1.1.1.1 /
  security / family / Mozilla, DNS.SB, DNSPub, AliDNS, AdGuard, NextDNS,
  RethinkDNS basic + sky, Tiar) — whenever the proxyPools table holds no
  pool of type `doh` at all. Previously the DoH transport shipped in the
  binary but an install had no pool until one was created by hand.
- **Idempotent and non-intrusive.** The seed runs once per boot through
  the database module and inserts only when no doh pool exists; an
  operator's own doh pool (even inactive or renamed) leaves the table
  untouched, and a second boot changes nothing. The insert goes through
  `InsertProxyPool`, so the row is identical to one written by the
  dashboard create form.
- **No routing is forced.** The seeded pool is never pinned to a provider;
  `providerStrategies` is not modified. Whether traffic uses the pool
  remains a dashboard decision.
- **Failure is not fatal.** A seed error is logged as a warning and boot
  continues, mirroring the vault migration's policy.
- Verified with table-driven tests in `internal/db`: seed on an empty
  table (exact name, resolver list and order, active, strict), a non-doh
  pool alone not blocking the seed, an existing custom doh pool left
  untouched, and double-seed idempotency.
