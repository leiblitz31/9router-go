### fix(translator): compare the trailing turn by value, not by JSON text

`sameTail` decides whether cleanup changed the tail of a conversation, and
`SanitizeClaudePassthrough` re-encodes the whole body when it says yes. It
answered the question by marshalling both messages and comparing the text, which
cannot work here: `encoding/json/v2` marshals a Go map in a non-deterministic
order (v1 sorted the keys, v2 does not — the package docs say so), so two
identical messages serialise differently about one time in ten.

Two consequences, one of them in production:

- `TestSanitizeClaudePassthrough_CleanBodyUntouched` fails intermittently — 17
  failures in 200 runs on `main`.
- A body that needed no cleanup was re-encoded anyway, on roughly one request in
  ten. That re-encoding is itself order-random, so the same conversation reached
  the upstream as different bytes each time, which defeats prefix caching on the
  Claude lanes.

`reflect.DeepEqual` on the decoded values answers the actual question and
serialises nothing, so it is also cheaper than the two marshals it replaces.

**Verified.** The new `TestSameTail_ComparesByValueNotByJSONText` fails on the
old code (`identical tails reported as different on attempt 0`) and passes here;
it runs 500 comparisons plus the two cases that must still count as changes.
`TestSanitizeClaudePassthrough_CleanBodyUntouched` passes 200 of 200 runs, and
`go test ./internal/translator/` passes in ten separate processes at `-count=1`,
which is how CI runs it.
