### qw-web: context externalization for oversized agent requests

- The lane flattens a whole OpenAI request into one user message, and
  chat.qwen.ai's risk control refuses bodies past roughly 128 KiB
  (RGV587). A Hermes agent session crosses that with its system prompt,
  tool catalogue and history combined — measured live on 2026-11-03: a
  ±177 KB body refused on every Hermes retry while small turns beside
  it succeeded.
- Oversized context now follows the site's own escape: the history (or,
  when the head/current turn dominates, the whole context) is uploaded
  as a text document — STS from /api/v2/files/getstsToken (v1 fallback),
  a V1-signed OSS PUT, /api/v2/files/parse with status polling — and
  referenced from the message's files array, while a budgeted live
  prompt (48 KiB: attachment notice, head, recent turns, current turn)
  stays inline. Design adapted from the measured behaviour of
  Rfym21/Qwen2API (90 KiB serialized trigger, prefix reuse, compaction
  doctrine) and YuJunZhiXue/qwen2API (v2 STS endpoint); the upload leg
  was verified live from this gateway before the planner was written.
- Baked history attachments are cached per connection (30 min TTL, max
  4) and reused when the next turn's history extends them, so a session
  pays one parse per history prefix, not one per turn. A parse rate
  guard (6 per 120 s) and a 3-strike WAF breaker (300 s) keep the
  upload leg from feeding the per-egress challenge counter.
- Fallbacks are honest: a tool-carrying request that cannot be
  externalized fails 503 instead of silently truncating the agent's
  history (the wrappers measured truncated histories replaying finished
  work); a plain chat compacts to an 84 KiB budgeted prompt; guest mode
  compacts plain chats and refuses oversized tool requests.
- Tool hardening from the same research: a runaway guard admits at most
  24 calls per turn and drops exact duplicates, and a request whose
  tool_choice requires a call is gated — buffered whole, one correction
  retry with a hint, then an explicit error rather than a fake stop.
- Verified: executor + providers suites green (the three pre-existing
  strict-proxy reds unchanged), a fake-upstream test drives STS → PUT →
  parse → reuse, and a live probe uploaded and parsed a document
  against the production account.
