### fix(chat): report a client abort as 499, not a bare 502

A client that hung up was recorded as a 5xx. `handleAccountFallback` returned the
raw forward error, which is not an `*upstreamError`, so the write sites in
`chat.go` fell through to their `StatusBadGateway` default — the caller received
`upstream error: context canceled` with a 502, and the request log recorded a 5xx
for a disconnect the gateway did not cause. Any 5xx-based alerting therefore
fired on client behaviour.

The repo already knew the right answer, which made this an inconsistency rather
than a gap: `StatusClientClosedRequest` is defined at `fallback.go:30`,
`fallback.go:725` computed 499 correctly for its own tracing span, and
`combo.go:625`/`:847` wrapped the same condition as an `*upstreamError` carrying
499. The identical abort was thus 499 on the combo path and in tracing, but 502
on the account-fallback path.

The abort is now wrapped the way `combo.go` does it, and the JSON body shared by
all three sites is single-sourced into `clientClosedBody` so they cannot drift.
The original error is not lost from diagnostics: it is still logged at
`fallback.go:870` before the wrap.

**Verified.** `fallback_client_closed_test.go` pins that a client abort yields an
`*upstreamError` carrying 499 while a live request keeps its upstream status, and
that `clientClosedBody` is byte-identical to the payload `combo.go` has always
sent. Run against the pre-fix code the first case fails with
`expected *upstreamError ..., got *fmt.wrapError: ... context canceled`.
`gofmt -l`, `go vet ./...`, `go build ./...` and `go test ./...` all clean.
