### GitHub embeddings catalogue: `text-embedding-ada-002`, not `text-embedding-3-large`

- Ported the catalogue half of upstream `decolua/9router` commit `51a1827e`
  (#4793, issue #400 parity). GitHub Models inference (`models.github.ai`) was
  retired on 2026-07-30 and Copilot embeddings now go to
  `https://api.githubcopilot.com/embeddings`, whose accepted embedding ids do not
  include `text-embedding-3-large`. Upstream swapped that registry row for
  `text-embedding-ada-002`; `text-embedding-3-small` stays.
- `internal/providers/registry_models.go`: the `gh` and `github` catalogue rows
  and their `ProviderModelKinds` entries now carry `text-embedding-ada-002`. A
  stale id here reaches `/v1/models` and the dashboard picker, so a client could
  select a model the Copilot endpoint rejects.
- `web/src/lib/models.ts`: the `gh` catalogue entry follows, so the dashboard
  stops offering the retired id.
- Verified: `go build ./...`, `go test ./...` (full suite) green;
  `TestGitHubEmbeddingModelParity` was proven RED before the change and GREEN
  after (`go test ./internal/providers/ -run TestGitHubEmbeddingModelParity`).
  `bun run build` and `make vet-svelte` green.

Closes #400.
