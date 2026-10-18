### fix(opencode): report a closed free tier as unavailable instead of a raw 403

When the OpenCode free-tier gate refuses to serve this traffic, the caller used
to receive the upstream `403 FreeTierError` verbatim — a wall of text telling the
user to download OpenCode, mid tool-call. A 403 reads as "you are not allowed",
so an agent loop treats it as fatal and stops.

The gateway now reports the condition instead of forwarding it:

- **`503` + `Retry-After: 30`** with `type: "free_tier_unavailable"` and a message
  naming the real cause and the real remedies. The status says "temporarily
  unavailable, come back", which is what a closed shared free tier is.
- **`Retry-After` is actually forwarded.** The error-to-response writer copied
  only `Content-Type` and dropped `ue.Header`, so a retry hint was silently
  discarded. It now forwards `Retry-After` and nothing else: copying the whole
  header map leaked the failed response's `Content-Length` and
  `Transfer-Encoding`, which made the reply advertise a body length it did not
  have and truncated it on the wire.
- **A refusal does not cool the connection down.** The gate says nothing about
  the account — the same connection serves the next request once the tier
  reopens — so the cooldown, backoff and model-lock path is skipped and the
  refusal is reported directly. Cooling a healthy account would turn a closure
  into an outage by skipping it for later requests. Scoped to the OpenCode
  providers; every other provider's 403 handling is unchanged.

**One retry, not a loop.** The gate *does* occasionally refuse a request whose
fingerprint is correct and serve the same request when repeated, so a single
repeat recovers that blip — verified end to end: first attempt refused, retry
served, client gets the completion. But while the gate is refusing on identity
(#367 measured every client variant refused) no number of attempts succeeds, so
the budget stops at two: worst case ~200 ms added, against the ~1.5 s a
5-attempt cycle cost. That cycle was in an earlier draft of this branch and was
replaced on review. Only the `FreeTierError` refusal is repeated — a quota 403
fails identically forever, and 401/429/5xx keep their existing handling.

Two related defects were found by testing rather than reading:

- **A truncated error body erased the status.** `doRequestOnce` returned a bare
  `fmt.Errorf` when reading a non-200 body failed, discarding the code, so a
  403 with a mis-declared `Content-Length` became an opaque 502 that no
  classifier — retry, failover or refusal — could recognise. `UpstreamError`
  now carries the status plus a `ReadErr` for the partial read.
- **The first header-copy loop broke an existing test**
  (`TestMiniMaxCodeCreditsRefusalFailsOver`, which asserted a 429 and got a
  truncated body). Caught by the integration suite; fixed by narrowing the
  forward to `Retry-After`.

Verified with `go build ./...`, `go test ./internal/proxy/...
./internal/handlers/chat/ ./internal/translator/ ./internal/providers/`,
`go test -tags=integration ./internal/integration/`, and
`golangci-lint run --new-from-rev=origin/main` (0 issues).

The end-to-end cases live in
`internal/integration/opencode_free_tier_closure_test.go` and drive the real
router against a fake gate that refuses every attempt:

- `TestFreeTierRefusal_ReportsUnavailableNotForbidden` — the client sees `503`
  with `Retry-After`, `free_tier_unavailable` names the condition, and neither
  the gate's error type nor its wording reaches the body.
- `TestFreeTierRefusal_DoesNotCoolTheConnectionDown` — two successive requests
  each dial the upstream, so the first refusal did not lock the connection.
- `TestFreeTierRefusal_SurvivesATruncatedErrorBody` — a mis-declared
  `Content-Length` still yields `503`, not an opaque `502`, because the status
  survives a partial read.

The gate's accept path is covered separately in
`opencode_zen_gate_simulation_test.go` (`TestInferenceGate_*`), which shapes a
harness body through the router, asserts the outbound tool set, and keeps a
negative control so the fake is not simply permissive.
