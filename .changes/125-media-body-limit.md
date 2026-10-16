### fix(router): per-route body limits, and a 413 that names the ceiling

`middleware.MaxBody(10MB)` was mounted on the root router, so the ceiling was
global: a 25MB video upload to `/videos/generations` or a long
`/audio/transcriptions` recording died at the JSON limit, and the handler
reported it as `400 "failed to read request body"` — indistinguishable from a
corrupt body. No path existed by which a media endpoint could get a higher limit,
which `max_body.go`'s own comment admitted ("Media endpoints may need higher
limits").

`internal/middleware/max_body.go` now resolves the ceiling per request:

- `MaxBodyByPath(default, rules)` picks the limit from the path; `MaxBody(n)` is
  it with no rules, so existing callers are unchanged.
- `MediaBodyLimitRules()` raises `/videos/generations`, `/videos/edits`,
  `/videos/extensions` and `/audio/transcriptions` to `MediaMaxBodySize` (128MB).
  Matching is by path **suffix**, so the bare route and its `/v1/` and `/api/v1/`
  spellings are covered by one rule.
- A rule may only raise: one lower than the default is ignored, so no caller can
  quietly weaken the protection the default provides.
- One middleware decides the ceiling instead of stacking two — a second
  `MaxBody` would wrap `r.Body` again and the smaller limit would still cut
  first, which is exactly why the media endpoints could not be raised before.

The resolved ceiling is recorded on the request context, and
`middleware.WriteBodyReadError` splits the read failure: a body that tripped
`http.MaxBytesReader` becomes a **413 naming the limit** ("request body too large:
this endpoint accepts up to 128MB", code `payload_too_large`), anything else keeps
the old 400. Applied to every inference handler that reads its own body — the
media and video endpoints named in the issue plus `/chat/completions`,
`/messages`, `/messages/count_tokens`, `/responses`, `/responses/compact`,
`/api/chat`, `/images/generations`, `/audio/speech`, `/audio/transcriptions` — so
the split is uniform rather than a special case for one lane.

**Verified.** `max_body_test.go` covers both ceilings at the boundary (exactly at
the limit passes, one byte over is refused), that a media ceiling clears the JSON
one, that the 413 names the limit and carries `payload_too_large`, that a
non-`MaxBytesError` is still a 400, and that the rules cover all four media paths
in all three prefix spellings. `internal/integration/body_limit_test.go` drives
`app.ProvideRouter` — the real middleware stack — and uploads 25MB to
`/v1/videos/generations` and `/v1/audio/transcriptions` (must not be a 413) while
an 11MB `/v1/chat/completions` body is refused with the 413.

**Residual risk.** 128MB is a judgement call: video generation takes 15-50MB clips
and 30-60 minute audio can exceed 100MB, but the ceiling is still a whole-body
buffer, so a hostile client can make the process hold 128MB per request. The
per-route shape is what makes that tunable per endpoint; a different number is a
one-line change to `MediaBodyLimitRules`.
