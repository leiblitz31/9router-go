### Fixed

- **A non-message error could end a whole import.** `POST /api/models/custom`
  answers a refusal with a typed `409`, and both bulk import paths awaited every
  save inside one `try`/`catch`. The first refused id therefore threw past the
  ids listed after it: the run stopped, the alert carried the raw 409 body, and
  every model below the collision was silently never imported. Each row is now
  attempted on its own: `importCustomModelsBestEffort` (new, in
  `web/src/lib/customModels.ts`) returns `{ imported, refused }`, both import
  paths report the refused ids with the server's own reason, and the ids that
  follow a collision are still imported.
- **A custom model id can no longer collide with a model alias or a combo.**
  `guardNameCollision` refused a custom model whose id matched a bare-name alias
  or combo, which is what produced the `MODEL_ALIAS_CONFLICT` above. A custom
  model id is only ever addressed as `<node-prefix>/<id>` (resolveModel step 1)
  and `/v1/models` publishes it in that same prefixed form, so it can neither
  occupy a bare name nor be shadowed by one — the two are distinguishable
  addresses, not one name resolving to the wrong target. The guard now refuses
  only the pair that really loses a name: a model alias vs a combo, in either
  direction, where the alias outranks the combo and the combo stops answering.
  The refusal was also order-dependent before — an alias written over an existing
  custom id was refused while the identical pair in the other order was accepted.
  A `/models` list carrying one already-aliased id (usually the same model on
  another node) imported nothing past it; now it imports in full.

### Verified

- `go test ./...`, including a rewritten `TestGuardNameCollision` that pins both
  directions of the surviving combo-vs-alias refusal, the four custom-model
  writes that are now accepted, and that a refused write still stores nothing.
- `bun test src scripts` — 405 pass, including a new
  `web/src/lib/customModels.test.ts` pinning that a refusal costs one row and not
  the batch.
- `web/e2e/modelImportNameConflict.test.ts` drives the reported flow against the
  real gateway: an OpenAI-compatible node whose `/models` lists an already-aliased
  id first, then a fresh one. Before this change the run stored **zero** rows
  (`Received: []`); now both ids are stored and no refusal dialog is raised.
- `bun run build`, `bun run ratchet:svelte` (81 errors, at baseline) and
  `bunx oxlint --deny-warnings` are clean.
