### fix(dashboard): mask connection secrets on rune boundaries, not bytes

The Connections and API-key pages rendered **0/0**. The response was `HTTP 200`
with the body truncated mid-record, followed by
`{"error":{"message":"internal error","type":"invalid_request_error","code":500}}`
— a payload no client can parse.

The mask helpers cut a secret with `s[:n]` / `s[len(s)-n:]`, which slices
**bytes**, not runes. When the visible prefix of a secret ended inside a
multi-byte rune the cut left invalid UTF-8 behind, and `encoding/json/v2`
rejects invalid UTF-8 *while encoding*. By then the `200` header is already on
the wire, so the client gets a half-written body plus the error sentinel
appended after it; the dashboard falls back to 0/0. Reproduced 100% against a
real `~/.9router/db/data.sqlite` holding two such connections.

`handlerutil.RunePrefix` / `RuneSuffix` / `RuneCount` is the single shared
helper for the cut (AGENTS.md §3.C — one general abstraction instead of four
inline patches) and now serves `maskConnectionSecret`, `maskConnectionName`,
`maskClientKey` and `MaskAPIKey`. Only the elision inside an already-200 payload
changes; no `/v1/*` or `/api/*` contract moves.

**Verified.** `internal/handlerutil/utf8mask_test.go` pins the helper's
rune-boundary contract; `internal/handlers/dashboard/connections_mask_utf8_test.go`
pins the failure mode itself — a connection whose secret ends in a multi-byte
rune must encode to valid UTF-8 instead of corrupting the whole payload.
`gofmt -l`, `go vet ./...`, `go build ./...` and `go test ./...` all clean.
