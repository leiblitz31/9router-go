### feat(mimo-web): tool synthesis for the Studio web surface

The MiMo Studio chat has no native tool channel, so mimo-web answered
agent requests as plain chat: the model explained it had no tools and
nothing ran. This teaches the declared tools as prompt text and recovers
the calls from the answer, the approach the deepseek-web executor
already proves — with one measured divergence. Where deepseek-web's
renderer asks for a JSON object, MiMo answers that protocol by narrating
an attempted call and its failure and emitting nothing callable; taught
DSML, the dialect it reaches for natively, its invokes come back
well-formed. The renderer is therefore mimo's own (DSML as the protocol,
the shared extractor still reads JSON as a fallback), while the tool
spec type, the extractor, the DSML scrubbers and the call-message
builder are shared with deepseek-web rather than re-implemented.

- `mimo_web_tools.go`: tool declaration parsing (both declaration
  spellings, `tool_choice: "none"` honoured), the DSML renderer with the
  anti-refusal rules, assistant-turn rendering for the flattened
  transcript (`[called …]` lines, echoed markup summarised instead of
  replayed), and reply finalisation (calls recovered with surrounding
  prose kept, stray markup stripped from plain answers).
- `mimo_web.go`: requests with tools get the rendered block ahead of
  the flattened transcript; the collect path finalises through the
  synthesis; a streaming request with tools buffers the turn (a call
  cannot be recognised, or kept out of the content channel, until the
  answer is complete) and then emits reasoning, content and tool_calls
  as single deltas. Requests without tools are byte-for-byte the old
  paths.
- `capabilities.go`: mimo-web models now declare `Tools: true`.

Verified: executor and providers suites pass (eight new mimo tests over
parsing, rendering, finalisation and transcript history). Live against a
scratch instance with a real Studio session: imperative agent requests
returned `finish_reason: tool_calls` with well-formed calls in both
non-streaming (4/4) and streaming (2/2) modes, a tool-result follow-up
turn answered from the result, and plain chat was unchanged. Known
limit: question-form requests ("what does file X contain?") still
sometimes draw a narrated failure instead of a call — the Studio side
appears to attempt such questions itself — while imperative agent
traffic, which is what agent clients send, is reliable.
