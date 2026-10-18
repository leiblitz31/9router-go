### fix(dashboard): list every routed model in the Available Models grid

The dashboard renders a provider's "Available Models" rows from
`web/src/lib/models.ts`, a hand-maintained copy of the provider catalog that is
**not** a projection of `internal/providers/registry_models.go`. Nothing compared
the two, so a model added to the Go registry alone was routable by the gateway
and absent from the only UI that configures it.

That is how `gemini-4-argon` shipped: #379 and #382 both grew the Go catalog and
neither touched `models.ts`, so the dashboard offered no Argon row at all —
`ag/gemini-4-argon`, `-high`, `-medium`, `-low` were all missing. #353 hit the
same shape for `gemini-web` and recorded it in its commit message; the next model
PR repeated it, which is the case for a test rather than a note.

- `web/src/lib/models.ts`: adds the missing rows for `ag`/`antigravity`
  (`gemini-4-argon` and its three tiers, carrying the `(level)` `upstreamModelId`
  upstream declares), `gemini`, `vertex`, `gc`/`gemini-cli`, and `opencode-go`
  (`claude-haiku-5-5`, `gemini-4-argon-contributor`, `longcat-2.5-preview-free`,
  `step-5-preview`, `beam-free`, all from #382). Two providers that had drifted
  earlier are repaired in the same pass: `cbcn` was missing `kimi-k2.8-preview`
  and `gemini` was missing the Gemini Live transcription STT row.
- `internal/providers/web_catalog_parity_test.go`: fails when a provider present
  in both catalogs serves an id the dashboard copy omits. It reads the
  TypeScript source rather than importing it, because the SPA is a separate build
  and pulling Vite and Bun into the Go test binary would cost far more than the
  check is worth. One direction only, deliberately: `models.ts` may legitimately
  list ids the Go registry does not, while the reverse silently hides a working
  model.

Verified: the new test fails on the pre-fix tree with the exact missing ids and
passes after (`105 providers present in both catalogs`, 0 drift). `bun run build`
emits a bundle containing all five Argon ids; `bun test src/lib/models.test.ts`
passes; `svelte-check` holds its 81-error baseline with 0 unresolved identifiers.
`bun test` across the SPA is 400 pass / 16 fail, identical to `main` — the
failures are e2e specs that need a running app.
