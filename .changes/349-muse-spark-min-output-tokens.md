### fix(executor): floor Muse Spark output budgets at 512 tokens

Muse Spark spends 570-1320 hidden reasoning tokens before emitting any completion
text, so a caller-set budget below that is consumed entirely by reasoning: the
upstream answers 200 with an empty message (which the gateway reports as
"Provider returned empty content") or stalls the stream until the watchdog fires a
502. Neither failure names the budget, so a working model looks broken. 512 is
upstream's measured boundary — max_tokens 64/100 → empty, 256/512/1024 → content
present — ported from OmniRoute `MUSE_SPARK_MIN_OUTPUT_TOKENS`
(`open-sse/executors/opencodeMuseSpark.ts`).

- `clampMuseSparkOutputBudget` raises a too-small budget in all three spellings an
  OpenAI-shaped request may use: `max_tokens`, `max_output_tokens`,
  `max_completion_tokens`. It only ever raises — an explicit larger budget and a
  request that set no budget stay byte-identical, because synthesizing a cap the
  caller never asked for would silently shorten real answers.
- It runs inside `normalizeMuseSparkResponsesBody`, and only for Muse Spark ids, so
  the sibling models that share the same Responses lane (`grok-4.6`,
  `gpt-5.6-luna`) keep their budgets.
- Reasoning-effort suffixes survive cleaning again: the `(high)`/`(medium)`/`(low)`
  a client appends to the model id is 9router request notation and never reaches
  the wire, but the intent it carried was being dropped with it. The callers now
  pass the model id as the client sent it and the sanitizer strips the prefix and
  the override itself. **Fill-only precedence**: an effort the body states
  explicitly is left exactly as written.
- `museSparkCleanModel` centralises the prefix/override stripping that both request
  paths previously duplicated.

**Verified.** `musespark_min_output_test.go` tables the clamp (below/at/above the
floor, all three spellings at once, absent and string budgets, sibling and
non-spark models), drives the real `normalizeMuseSparkResponsesBody` with client
JSON for both the raised and the untouched case, and checks suffix precedence both
ways. The pre-existing `TestForwardOpencode*MuseSpark*` and
`TestNormalizeMuseSparkResponsesBody_ReasoningAndToolChoice` suites still pass.
