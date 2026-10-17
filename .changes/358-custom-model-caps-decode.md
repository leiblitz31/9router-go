### 🐛 Custom model rows no longer lose their declared limits when `caps` holds a non-boolean value

A `customModels` row is decoded into `map[string]bool`, but rows written by
other builds — and by hand — carry non-boolean metadata in that block. The
real-world case is a bridge-backed node whose row holds
`"thinkingFormat":"effort"` inside `caps`. `encoding/json/v2` aborts the object
at that member, so every field *after* `caps` was silently dropped;
`contextWindow` and `maxOutput` sit there, so the row read as "no limits
declared".

The consequences were invisible but large:

- `/v1/models` and `/v1/models/info` published the substring table's floor
  (`128000 / 4096`) for an endpoint that had declared `1048576 / 65536`.
- `/api/models/caps` published the same wrong numbers, so the dashboard showed
  a 128k window on a 1M model.
- Clients that budget context from the discovery response (OMP, pi, anything
  reading `context_length`) capped themselves at 128k and compacted far too
  early. The request path never truncated — only the advertised metadata lied.

`CustomModel` now decodes `caps` through a small `UnmarshalJSON` that reads the
row's ids, name and declared limits first and only then filters `caps` down to
boolean flags — the same policy upstream's `sanitizeCaps` applies on write.
Non-boolean members are dropped, the row itself always survives, and a `caps`
that is absent, `null` or not an object leaves the row's limits intact.

`gemini-argon` / `gemini-4-argon` also gained the rows upstream already carries:
the `*gemini-4*` capability pattern (vision + reasoning + search, `gemini-level`
thinking, thinking not disableable, 1048576/65536), the same ids in the token
limit table, and the `["low","medium","high"]` thinking levels — Gemini 4
rejects `minimal` with a 400 and cannot turn thinking off. Without those rows
any Gemini 4 id that lost its custom row fell through to the 128k/4096 floor.

**Verification:** `go test ./internal/db/ ./internal/providers/
./internal/handlers/chat/ ./internal/handlers/dashboard/` — 5 new tests, all of
which fail without the fix (`declared limits = (0, 0)`, `max_completion_tokens
= 4096`, `thinkingFormat = ""`). Replayed end to end against a 9router 1.9.11
binary and a gsb-shaped upstream: the four petafora `customModels` rows
publish `128000/4096` before and `1048576/65536` after, on `/v1/models`,
`/v1/models/info` and `/api/models/caps`.
