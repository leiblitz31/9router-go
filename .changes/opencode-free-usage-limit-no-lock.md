### OpenCode free-tier 429 (FreeUsageLimitError) no longer locks the account

- **The free-tier refusal matcher learns the zen 429 shape.**
  `isOpencodeFreeTierRefusal` recognised only the inference gate's
  403 `FreeTierError`. The zen endpoint answers the same condition with
  429 `FreeUsageLimitError`, which fell through to the generic retryable
  path: `ClassifyError` treated it as an account rate limit and wrote a
  per-model lock plus the account-scoped `rateLimitedUntil` cooldown —
  observed live at the maximum backoff, 7200s, for a free window that
  reopens in seconds to minutes. One transient 429 therefore read as a
  two-hour outage for every model on the connection, and a client retry
  loop kept re-arming the lock. The matcher now accepts the 429 shape
  (error type matched, not prose), so both fallback paths take the
  existing no-lock branch and the client receives the honest
  503 + Retry-After free-tier response.
- **The combo path gets the same guard.** `comboLockRetryable` locked on
  any retryable error without consulting the refusal check; it now
  excludes the connection for the current request and returns, mirroring
  the single-provider path.
- Verified with table-driven unit tests in
  `internal/handlers/chat/fallback_free_tier_test.go` using the real
  captured 403/429 bodies: both gate shapes classify as refusals for the
  opencode provider family only, while a 429 with any other error type,
  an auth 403, and other providers keep the normal locking path.
