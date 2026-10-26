### Fix poolside 400 "duplicate field `reasoning_content`" on sessions with history

- Poolside's upstream deserializer maps the `reasoning` message key onto
  the `reasoning_content` field, so a message carrying both is rejected
  with HTTP 400 "duplicate field `reasoning_content`". Hermes-style
  clients echo back streamed `reasoning_content` on assistant turns while
  also carrying their own `reasoning` field, so every later turn of a
  Laguna session failed — short fresh prompts worked, long sessions did
  not, and both `ps/laguna-s-2.1` and `ps/poolside/laguna-s-2.1` failed
  identically because they reach the same upstream.
- `ForwardPoolside` now strips the `reasoning` key from any message that
  also carries `reasoning_content`, before the shared OpenAI passthrough.
  Messages with only one of the two keys pass through byte-identical, and
  the strip composes with the existing `poolside/` model rewrite in the
  same request. No other provider's path is touched.
- Verified: live repro through the router returned the exact 400 for a
  history message with both keys (and 200 for either key alone) before
  the fix; unit tests pin the strip (both keys, either key alone, no
  messages, invalid JSON, non-object messages) plus a fake-upstream
  executor test asserting the wire body and model rewrite together.
