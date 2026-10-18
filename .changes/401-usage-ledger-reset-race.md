### Usage ledger: a quota reset that lands mid-prune is no longer re-banked

- `internal/db/usage_retention_ledger.go`: `PruneUsageHistory` folded each
  `usageHistory` chunk against the reset marks it snapshotted before the prune
  began, then carried that spend in a later transaction. A quota reset that
  committed in between raised the key's mark past the rows being deleted and
  cleared its carryover — but the fold still used the stale mark, so the spend
  the reset had just forgiven was written back to the key's budget and charged
  again.
- `deleteLedgerChunk` now re-reads the watched keys' reset marks inside the same
  transaction that writes the carryover and deletes the rows
  (`ledgerWatchIndex.refreshMarks`), so "which mark decided what to carry" and
  "which rows are gone" are the same instant. The key list is not refreshed: a
  key created after the prune started cannot own a row this pass is deleting.
- Verified on top of `main` after the ledger branch merged: the race test
  (`internal/db/usage_retention_ledger_race_test.go`) was proven RED against
  `main`'s `deleteLedgerChunk` — `usage after a reset that landed mid-prune =
  500, want 0` — and GREEN with the fix; `go build ./...`, `go test ./...` and
  `go test -race ./internal/db/` are green.
