### Fix cloudflare-ai 400 code 8007 on multi-system payloads

- Workers AI's OpenAI-compatible chat endpoint accepts at most one system
  message, and it must be first: a second system message — even immediately
  after the first — is answered with 400 code 8007 "System message must be
  at the beginning." Hermes sends two leading system messages, so every
  such session failed on `cf/*` while the same payload worked elsewhere.
- Added a dedicated `cloudflare-ai` executor (`ForwardCloudflareAI`) that
  merges every `role:"system"` message into a single leading system message,
  in order, joined with a blank line, before the shared OpenAI passthrough.
  Bodies with exactly one leading system message pass through byte-identical;
  unparseable bodies and unmergeable system content are forwarded untouched
  rather than silently dropped. No other provider's path is touched.
- Verified: unit tests for the merge (two leading systems, system after a
  user turn, block-array system content, no-system and unmergeable passthrough)
  plus a fake-upstream executor test; direct upstream check on
  `@cf/qwen/qwen3.8-27b` confirmed 1 system → 200 and 2 systems → 400/8007
  before the fix.
