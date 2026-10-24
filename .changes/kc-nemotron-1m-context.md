### fix(providers): declare Kilo Code's real 1M context for the free Nemotron models

The models.dev-synced catalog publishes a 128000 context for Kilo Code's
`nvidia/nemotron-3.5-super-vl:free`, `nvidia/nemotron-3-ultra-550b-a55b:free`,
and `nvidia/nemotron-3.5-lightning:free`, and `/v1/models` repeated that
figure in the `capabilities` block. The gateway itself serves all three with
a 1M-token cap: its own error message names the limit ("This endpoint's
maximum context length is 1000000 tokens", returned for a ~1.19M-token
request), and 145k/300k-token prompts complete normally.

`providerTokenLimits` in `internal/providers/capabilities.go` now declares
`{1000000, 4096}` for the three ids under both provider key forms callers
pass (`kilocode` and its `kc` alias — the lookup does not resolve aliases).
`GetCapabilitiesDetailForModel` consults this map before the synced catalog,
so the published capabilities now agree with the operator-declared
`customModels` rows that already fixed the top-level `context_length`.

Verified: `go test ./internal/providers/...` passes; the rebuilt gateway's
`/v1/models` reports `context_length` 1000000 and
`capabilities.contextWindow` 1000000 for all three models, and live chat
completions through `kc/*` still succeed.
