### fix(auth,opencode): proxy pool fallback for no-auth providers

Upstream decolua/9router#4998 parity (full port). No-auth providers (e.g.
`opencode`) with proxy pool rotation now fall back to the next pool when one
returns 403/429/5xx, instead of surfacing the error immediately:

- `ResolveProviderProxyPoolIDWithExclusions`: new variant accepting
  `excludeIDs`; filters out failed pools during rotation (both `proxy:${id}`
  and bare forms). Returns "" when all pools are exhausted (no leak to direct
  egress). A pinned single pool that was excluded also returns "".
- Virtual no-auth connection is now tagged `proxy:${poolId}` (instead of bare
  `noauth`) when a pool is selected, with the pool name as the display name
  for logging. Bare `noauth` is kept when no pool is configured.
- `comboLockRetryable` (the `markAccountUnavailable` equivalent): `proxy:*`
  connections allow fallback on 403/429/5xx via `ClassifyError` and are added
  to `excludeIDs` so rotation skips them; no DB lock is attempted (virtual
  connection has no row). Bare `noauth` keeps `shouldFallback=false`.

Verified with table-driven unit tests: exclusion filtering (bare + tagged
forms), connection ID tagging with pool name, and excluded single-pool
fallback to bare noauth.
