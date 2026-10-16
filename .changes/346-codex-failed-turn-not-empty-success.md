### fix(codex): a failed turn is no longer served to the client as an empty 200

Claude Code (and any other client) received an empty response when the Codex
upstream rejected a request — a tool-schema `pattern` it refuses was the case
reported in #334. The upstream's reason never reached the client, and because
the turn was closed with a fabricated success terminal, combo fallback stopped
on the model that had just failed.

**Cause.** The failure arrives as an *event on an HTTP 200 stream*, not as an
HTTP status, so by the time it is known the status line is long gone.
`ProcessCodexEvent` already recorded such an event on `state.UpstreamErr` —
its own comment says "handleCodexStream converts to an UpstreamError after the
scan" — but only the non-streaming path did. On the streaming path, which is
the one Claude Code uses, nothing read `state.UpstreamErr`:

- a `[DONE]` in the stream wrote the success terminal (`finish_reason: "stop"`,
  or `stop_reason: "end_turn"` for a Claude client) and the `[DONE]` right
  there, before any error check could run;
- when no `[DONE]` arrived, the same fabricated terminal was written afterwards.

Both read to a client as a completed answer carrying no content.

**Fix.** The streaming path now ends a failed, content-free turn the way the
rest of the gateway already ends a failure after HTTP 200 — with its own
in-band error frame (`proxy.BuildStreamErrorBytes`, the shape `openai.go` and
`bedrock.go` use) and a returned `*proxy.UpstreamError`, so the failure row is
written and fallback rotates instead of settling.

- `codexFailedEmptyTurn` is the one predicate for "the upstream failed and
  produced nothing". An error event *after* content is deliberately not that
  case: those turns keep the partial reply and stream normally.
- `response.failed` is now recorded as well, and `normalizeCodexFailureEvent`
  rewrites it into the `{"error":{...}}` shape the reader understands, so the
  upstream's real message survives.
- `codexEventErrorInfo` is shared by the returned error and the in-band frame,
  so the client's error line and the failure row cannot disagree.
- The `pattern` stripping Codex does need (`\p{...}` Unicode property escapes)
  was checked against upstream and is already at parity: upstream's
  `open-sse/utils/codexToolSchema.js` strips only those, and lookarounds are
  **not** stripped there either. Nothing was loosened; this issue is the
  error path, not the sanitizer. Tool-schema validation semantics are
  unchanged.

**Verified.** `internal/proxy/executor/codex_failed_turn_test.go` covers both
clients × both event shapes (`error` and `response.failed`), the precedence case
(an error after content is not replaced), and a healthy turn still terminating.
`internal/integration/codex_failed_turn_test.go` boots the real router against a
fake upstream and pins the client-visible contract for `/v1/messages` and
`/v1/chat/completions`. Both fail against the pre-fix code, and both pass with
it. `go test ./...` and `make test-integration` are green.
