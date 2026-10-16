### fix(combo): stop retrying a model that cannot hold the request

A combo member that answers 400 because the prompt exceeds its context window is a
fact about the model, not the account — every connection to it replies the same
way, and a larger member of the same combo can serve the prompt. The loop treated
it as an account fault and spent its whole connection budget on it: with
`connAttempt < 10`, a too-small first member was asked **10 times** before the
turn moved on.

- `internal/providers/errorclassify.go` gains `ModelFitErrorPatterns`,
  `ModelFitStatusCodes` and `IsModelFitError(statusCode, body)` — a typed
  classifier rather than a `strings.Contains` scattered through the loop. It is
  gated on status (400/413/422) so a provider's prose cannot trigger it on an
  unrelated failure, and matched case-insensitively against the body.
- Both combo loops (`handleComboFallback`, `handleMessagesComboFallback`) break to
  the next member on a fit error. Breaking rather than `continue` is the fix: it
  skips the remaining connections of that model, and it also skips
  `comboLockRetryable`, so a healthy account is not cooled down for a request that
  was simply too big.

When every member is too small the client still receives the upstream 400
verbatim, so the operator can read the model's limit.

**Verified.** `model_fit_error_test.go` covers the OpenAI/Anthropic/Gemini
wordings, case-insensitivity, the 413/422 carriers, and the negatives (a
malformed-body 400, a 500 whose text merely mentions a context window, an empty
body, a 401). `internal/integration/combo_context_overflow_test.go` drives the real
gateway with a fake upstream: the combo advances to the larger member, the
too-small member is asked **once** (this assertion failed before the change — it
saw 10), and an all-too-small combo returns the upstream 400 with the limit text
intact.

**Scope note.** The issue's headline symptom — the combo returning the 400
directly without trying later members — did not reproduce: the fallback already
advanced. The measurable defect was the wasted connection budget, and that is what
the predicate and the `break` remove.
