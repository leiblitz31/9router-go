### perf(db): order usageHistory reads by timestamp so idx_uh_ts is usable

`GetUsageHistorySince` and `GetRecentUsageHistory` filtered and ordered
`usageHistory` by `rowid`, which no index can satisfy — SQLite ran a full table
scan whose cost grew with the ledger, while the dashboard's Overview tab polls
`/api/usage/stats` every 5 seconds. `EXPLAIN QUERY PLAN` measured 9.5ms on a
100k-row table against 0.0ms for the same query ordered by `timestamp`, which
`idx_uh_ts` covers. `GetUsageHistoryBetween` had the same defect.

All three now order by `timestamp` (DESC for the newest-first readers, ASC for the
delta fold), and the three statements are named constants so the plan the
dashboard pays for is pinned to the SQL the functions actually run instead of to
a copy that can drift.

**Verified.** `internal/db/usage_query_plan_test.go` builds the production schema
via `EnsureCoreSchema`, seeds 5000 rows, and runs `EXPLAIN QUERY PLAN` on the
three constants: no plan row may contain a bare `SCAN usageHistory`, and each plan
must name `idx_uh_ts`. A second test writes rows whose insertion order is the
reverse of their timestamp order — the shape a late retry produces in production
— and asserts `GetUsageHistorySince` returns newest-first,
`GetRecentUsageHistory` returns the two newest, and `GetUsageHistoryBetween`
returns oldest-first.

**Behaviour note.** Rows sharing an identical `timestamp` may now come back in a
different relative order. Every writer stamps RFC3339 or
`2006-01-02T15:04:05.000Z`, both zero-padded and lexicographically sortable, so
the ordering callers depend on is unchanged; the fold in `usage_window_cache.go`
aggregates and is order-insensitive within a millisecond.
