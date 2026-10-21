### The 429 nonsense-window test now expects the 120s lock cap

- **Stale expectation updated.** The `429 nonsense window is capped`
  case in `rate_limit_failover_test.go` still expected a 429 carrying a
  72h reset window to be capped at `maxResetCooldown` (2h). Since the
  429 lock cap (`max429LockCooldown`, 120s) landed, `retryableCooldownSec`
  caps every 429 wait at 120s, so the case failed against the intended
  behaviour. The case now expects `max429LockCooldown`, referencing the
  constant the same way the old case referenced `maxResetCooldown`.
  Non-429 cases in the same table — including the 503 quota-reset
  readings that legitimately use the 2h bound — are unchanged.
- Verified with `go test ./internal/handlers/chat/ -run
  TestRetryableCooldownSec -count=1`: the updated case passes alongside
  the dedicated cap tests in `retry_after_cap_test.go`.
