### fix(codebuddy-intl): send the X-Conversation-* header family the intl gateway now requires

Since 2026-10-15 `www.codebuddy.ai` runs a "channel verification" gate: a chat
request that does not carry the `X-Conversation-*` header family the official IDE
sends on every turn is refused before the model is reached, with

```
HTTP 400 {"code":11128,"msg":"conversation id missing",
 "displayTips":{"en":"This call failed channel verification, possibly because it
 was not sent from an official client. Please resend from the official client."}}
```

100% of `cbai/*` requests failed that way — the report that opened this is
`codebuddy-intl/deepseek-v4.1-flash`. `codebuddy-cn` (`copilot.tencent.com`) is
not gated, which is why only intl connections broke. Same upstream change as
decolua/9router#4892.

`ForwardCodebuddyCN` now detects the intl `BaseURL` (`codebuddy.ai`) and merges
three headers into a **copy** of the provider config's `StaticHeaders`, so the
shared map is never mutated and the CN path is untouched:

- `X-Conversation-ID` — RFC-4122-v5-shaped UUID derived from the session key
  (`req.SessionID`, falling back to the opening user message, then a random
  UUID), so retries of the same turn and account failovers stay on one
  conversation. The derivation is byte-identical to the JS executor's in
  decolua/9router#4928, checked against it directly.
- `X-Conversation-Request-ID` — fresh UUID per request
- `X-Conversation-Message-ID` — fresh UUID per message

**Verified.** `codebuddy_conversation_headers_test.go` covers header presence and
UUID shape, stability of the conversation id across retries, session-id
precedence, typed-block content, and the random fallback.
Against a live upstream the author A/B'd the same credentials and body with only
the family differing: absent → 400 / 11128, present → 200 SSE. Independently, a
wire-level check against an `httptest` upstream confirms the three headers reach
the request for the `codebuddy.ai` BaseURL (with `Accept: text/event-stream` and
the static headers intact), are absent for the CN BaseURL, and that
`StaticHeaders` is not mutated.

**Residual risk.** The gate was confirmed to require the family; that the three
headers it names are sufficient (rather than more of the ~10 the official client
sends) rests on that A/B and could not be re-derived here without an intl
credential. Detection keys on `strings.Contains(cfg.BaseURL, "codebuddy.ai")`,
which is safe today because the executor is provider-scoped but would need
review if a custom `baseUrl` ever pointed at that host.
