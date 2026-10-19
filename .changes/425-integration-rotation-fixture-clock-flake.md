### Fixed

- **The account-rotation integration fixture no longer depends on the wall
  clock.** `TestRetryableUpstreamErrorRotatesAccount` failed on `main` (CI run
  39201511743, commit `1c4fb08`) with "throttled upstream received 0 requests,
  want 1": `Env.AddConnection` stores every connection at `priority := 1`, so
  the fixture's two DeepSeek accounts tie, and `connectionSortOrder` breaks the
  tie on `updatedAt DESC` — which `nowUTC()` writes at whole-second resolution
  (`time.RFC3339`). Whether the throttled or the healthy account was picked
  first came down to whether the two inserts straddled a second boundary, which
  is why it passed for so long. No production behaviour changed: newest-updated
  wins among equal priorities is the documented rule and mirrors upstream.

### Changed

- `Env.AddConnectionWithPriority` added, with `AddConnection` delegating to it at
  priority 1, so a fixture that needs a deterministic first pick can state it.
- The rotation fixture and `TestForceFallbackUsesAccountThatFreesUpFirst` (whose
  comment already claimed an order it was not given) now pass distinct
  priorities.

### Verified

- `go vet ./...`, `go vet -tags=integration ./internal/integration/...`, and
  `go test ./...` pass.
- `go test -tags=integration -race -count=1 -timeout 10m ./internal/integration/...`
  — the CI job's own command — passes (`internal/integration` 196.9s,
  `bootfx` 2.0s) on the merged tree plus this change.
- A probe on the merged tree reproduced the CI signature exactly: two
  equal-priority inserts in the same second sent the first request to the
  throttled account (`throttled=1 healthy=1`), inserts straddling a second
  boundary sent it to the healthy one (`throttled=0 healthy=1`). With distinct
  priorities both runs pick the throttled account.
- `internal/db/connections_order_test.go` pins the rule the flake tripped over
  with fixture-written timestamps: priority wins, then most recently updated
  first. Flipping the tie-break to `updatedAt ASC` fails it, and so does
  dropping priority from the `ORDER BY`.
