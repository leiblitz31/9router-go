### Fixed

- **A converted [OI] conversation no longer forwards a turn Anthropic rejects
  for having no content.** `EnsureClaudeMessagesFor` — the conversion behind the
  minimax-code and opencode-zen Messages lanes — passed the client content
  arrays through unchanged, so a turn the client left empty (`content: []`, or a
  whitespace-only string) reached the provider as-is and 400ed the whole
  request, while the same conversation on the Claude-client path was cleaned by
  `SanitizeClaudePassthrough`. The rule now lives in one exported helper,
  `translator.DropNonContentfulClaudeMessages`, called by both paths, and an
  emptied trailing turn is replaced by the `Continue.` placeholder so the body
  cannot end on an assistant turn.
- A conversation whose every turn is contentless keeps its turns: dropping the
  last one too would trade an invalid empty-content turn for an equally invalid
  empty `messages` array, and neither shape can be answered.

### Verified

- `go test ./...` and `go test -tags=integration ./internal/integration/...`
  pass.
- Table-driven coverage: `internal/proxy/executor/claude_empty_content_test.go`
  (seven cleanup cases plus the fully-emptied one) and
  `internal/translator/claude_empty_content_test.go` (six cases on the shared
  helper, including the `changed` flag its callers rely on). Reverting the
  executor call fails four subtests; disabling the filter fails the translator
  cases, so both sides of the shared rule are pinned.

