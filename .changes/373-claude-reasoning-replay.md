### fix(executor): keep OpenAI-only keys off the Claude messages

A replayed assistant turn used to carry the client's remaining keys into the Claude
body. `convertOpenAIMessagesToClaude` copied every key of a message — the
`tool_calls` branch everything except `tool_calls`/`content`, the fall-through the
map whole — and the top-level cleanup in `ensureMessagesMaxTokens` never looked at
messages. Anthropic answers a message key it does not define with
`400 messages.<n>.reasoning_content: Extra inputs are not permitted`, and because
that turn stays in the client's history, every later request of the conversation
failed too.

Normal use reaches it, not a hand-crafted payload: reasoning clients (pi, the
OpenAI SDKs, Hermes) echo back the `reasoning_content` this proxy streamed to them.
#290 is the request-side half that made Claude start producing the field, so this is
its second turn. The text cannot be turned into a `thinking` block instead — the
signature Anthropic needs to accept one back is not in the OpenAI shape.

Messages are now rebuilt from an allowlist of the keys a Messages API message
object defines (`role`, `content`); `SanitizeClaudePassthrough` applies the same
rule to Claude passthrough bodies from mixed combos. This matches upstream
`open-sse/translator/request/openai-to-claude.js`, which rebuilds every message as
`{role, content}`.

**Verified.** The Claude upstream returned `400 messages.1.reasoning_content: Extra
inputs are not permitted` for a replayed reasoning turn before the change and `200`
after, on a build of this branch over `v1.9.13` with a real Claude OAuth connection.
A turn with `reasoning_effort: high` still returns `reasoning_content` (185 chars)
plus its `tool_use`, and replaying both with the `tool_result` answers normally; a
real pi session with thinking `high` that used to die on turn 2 now completes. New
tests assert the body the upstream sees, in both conversion paths, including that
the tool_use/tool_result turns survive. `gofmt`, `go vet ./...` and
`go test ./...` clean.
