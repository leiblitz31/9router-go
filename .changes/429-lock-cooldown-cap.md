### A 429 can no longer lock an account for more than 120 seconds

- **The honoured Retry-After is capped for 429 locks.**
  `retryableCooldownSec` took a 429's Retry-After at face value up to
  `maxResetCooldown` (2h). OpenCode's free tier names a ~2h wait for a
  window that reopens in minutes, so a single transient 429 parked the
  model — and, via the account-scoped `rateLimitedUntil`, every model on
  the connection — for the full two hours; a client retry loop then kept
  re-arming the lock (observed live, 2026-10-21, three times in one
  afternoon). A 429 lock is now capped at `max429LockCooldown` (120s)
  whether the duration comes from the upstream's Retry-After or from a
  maxed classifier backoff. Short waits are still honoured exactly, so
  the anti-hammering behaviour inside a burst is unchanged.
- **Non-429 statuses are untouched.** Quota-reset readings (e.g. a
  provider whose window genuinely resets in hours) keep the
  `maxResetCooldown` bound they have always had; the cap applies only
  where the status is 429. Together with the free-tier refusal matcher
  (`FreeUsageLimitError` never locks at all), the worst case for a
  rate-limited free model is now a two-minute pause that a client's own
  retry loop outlives.
- Verified with table-driven unit tests in
  `internal/handlers/chat/retry_after_cap_test.go`: a 7200s Retry-After
  caps to 120, 30s and 120s waits pass through, a maxed 300s classifier
  base caps to 120 for 429 only, and non-429 cooldowns (3600s, and 3h
  bounded to the 2h global cap) are unchanged.
