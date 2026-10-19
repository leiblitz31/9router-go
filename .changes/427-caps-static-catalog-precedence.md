### Fixed

- **A model-list request no longer republishes a custom row's caps under an id
  the static catalog owns.** `GET /v1/models` publishes every custom row's saved
  caps onto the process-wide `customCaps` map
  (`internal/handlers/chat/models_list.go:795`), and
  `GetCapabilitiesDetailForModel` consults that map before the registry
  (`internal/providers/capabilities.go:1065`). #422 filtered the colliding ids
  inside `HandleGetModelCaps`, which fixed the dashboard route and left the chat
  route open: one list request — what an SDK does at startup — made caps answer a
  custom row's 4096 for a model the catalog declares at 1048576, from the first
  call onward and for the lifetime of the process.
- The refusal moves to the single writer, `providers.SetCustomModelCaps`, so the
  invariant is stated once and every publisher obeys it. The id is matched
  exactly, and by its bare form when the caller passes a provider-qualified one;
  a provider node prefix has no static catalog and is unaffected.

### Verified

- `go vet ./...`, `go vet -tags=integration ./internal/integration/...`, and
  `go test ./...` pass; `go test -tags=integration -race -count=1 -timeout 10m
  ./internal/integration/...` (the CI job's own command) passes.
- Both tests are fix tests and were replayed against pre-fix `origin/main`
  (`1c4fb08`), where they fail with the reported signature: the registration
  table in `internal/providers/capabilities_static_skip_test.go` reports
  `stored = true, want false` for a static id, and
  `internal/integration/caps_static_precedence_test.go` reports
  `contextWindow = 4096, want the static catalog's 1048576` on both calls after
  `/v1/models?all=1`.
- Mutation check: deleting the refusal fails both tests. Deleting the
  `ResolveAlias` lookup in the helper changed nothing (`GetProviderModels`
  resolves provider aliases itself), so that branch was dropped rather than kept
  as decoration.
