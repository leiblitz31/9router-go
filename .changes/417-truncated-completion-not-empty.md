### fix(proxy): treat a max_tokens cutoff as an answer, not an empty 200

A Claude turn that stopped because it hit `max_tokens` before writing any text
was reported as `502 upstream answered 200 without a completion`, and the 502 put
the whole connection in cooldown.

It happens with thinking on and a small `max_tokens`: the model spends the whole
budget on an empty thinking block, so the turn has no text and no tool call.
Anthropic answers `200` with `stop_reason: "max_tokens"`, a valid, short turn.
`missingCompletion` only accepted content, tool calls, reasoning or a refusal, so
`EmptyUpstreamError` read the cutoff as a missing answer. Two consequences: the
streaming path already served the same turn as `200` with `finish_reason:
"length"`, so only non-streaming clients got the 502; and the retryable 502
reached `LockConnectionModel` and `LockConnectionRateLimit`, so repeated hits
backed off 2s, 4s, 8s, 16s, 32s and a different model on the same account failed
with `no available connections for provider … (all in cooldown)`.

`finish_reason: "length"` (Chat Completions) and `stop_reason: "max_tokens"`
(Claude Messages) now count as an answer, so the client receives the truncated
turn and can raise `max_tokens`, and the cooldown is not touched. A response
that is empty for any other reason (`stop`, `end_turn`, `content_filter`,
`choices: []`, a blank body) still fails over exactly as before.

**Verified.** On a build of this change over `v1.9.13` against a real Claude OAuth
connection (`cc/claude-sonnet-5-5`), the request that returned the 502 and locked
the account now answers `200` with `finish_reason: "length"` three times in a row,
a different model on the same account still answers, and the router logged no
`connection locked`. Rolling back to the previous image reproduces the 502.
