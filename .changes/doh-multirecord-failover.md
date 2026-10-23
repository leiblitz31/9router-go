### DoH pools: dial every A record, and collapse concurrent lookups

- **A DoH pool picked one address and had no failover, so `cl`, `kc` and `kr`
  failed at random through it.** `resolveDialTarget` returned a single record
  from the resolver's answer and `dialTunnel` dialed exactly that one. Every
  provider the DoH pool serves is multi-homed, and their answers carry records
  this network cannot reach: measured live, `runtime.us-east-1.kiro.dev`
  published 8 A records of which 5 answered the proxy's CONNECT with
  `502 Bad Gateway`, and `api.kilo.ai` published 2 of which 1 did. Each request
  therefore drew a dead address about 62% (kiro) or 50% (kilo) of the time and
  paid a full CONNECT timeout before failing outright — the reported symptom
  was `kr` hanging for three minutes and `kc`/`cl` reporting transport errors
  that looked like account problems. `resolveDialTargets` now returns the whole
  answer in rotation order and `dialTunnel` walks it, so a dead record costs one
  attempt instead of the request. Measured after the change: 8/8 concurrent
  requests answered on all three hosts in 3.6–5.6s, against 1/6 and 0/6 before.
- **Each attempt is capped only when there is somewhere to fail over to.** With
  a single record the caller's own deadline stands, exactly as before; with
  several, `dohDialAttemptTimeout` (8s, against 0.4–1.9s for a healthy CONNECT
  here) bounds one address so the next one still gets a turn.
- **Concurrent misses for one host now collapse into one query.** A burst of
  requests to the same provider — a retry storm, or the dashboard's parallel
  probes — used to fire one DoH query per request, and the resolvers answered
  the pile-up by timing out every attempt: six concurrent requests to
  `api.cline.bot` failed with `context deadline exceeded` on all four resolvers
  while the same host answered in ~1.2s alone. `resolveA` runs the query through
  a `singleflight.Group` keyed by resolver identity plus hostname, so the second
  and later callers wait for the first answer instead of starting their own.
  The shared query runs on `context.WithoutCancel` with its own budget, so a
  leader that disconnects cannot tear down the answer its waiters are queued
  behind — the same failure the live catalog flight had to fix.
- **The diagnostic note names the record that accepted.** With failover the
  address dialed is no longer the address asked about, so `rememberDoHResolution`
  is written from the successful target rather than from the first candidate;
  a later TLS error otherwise points the operator at a host the request never
  reached.
- Verified with four new tests: `TestDoHResolveDialTargetsReturnsEveryRecord`
  (all records returned, cursor rotates, literal IP still a single passthrough),
  `TestDoHDialFailsOverAcrossRecords` (a fake proxy refuses the first record and
  accepts the second — reverting the loop to `targets[:1]` fails it with
  `CONNECT 203.0.113.1:443 refused: 502`, so the test is not vacuous),
  `TestDoHDialRecordsTheAddressThatAccepted`, and
  `TestDoHResolveACollapsesConcurrentMisses` / `...FlightSurvivesLeaderCancellation`
  (8 concurrent callers produce exactly 1 query; a cancelled leader still leaves
  the follower a working answer). Live on the deployed binary: `kc` answers 200
  in 2.5s through the DoH pool, `cl` reaches Cline and returns its real
  `insufficient_credits` envelope in 2.6s, and `kr` reaches the Kiro gateway in
  ~5s — its remaining 403s are revoked/suspended accounts ("Bad credentials" on
  refresh), not transport.
