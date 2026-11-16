### Merge upstream 1.9.17-exp.1 into the fork (user-ordered, supersedes cherry-pick-only for this branch)

Branch `integrate/merge-1.9.17` merges `f8dba9db` (fork/merge/upstream-1.9.17,
VERSION 1.9.17-exp.1) into main `5ccbeb85` wholesale — 1,147 files — on the
operator's explicit order after the consequences were laid out. 105 conflicted
files resolved: the merged branch's version wins wherever main held a
cherry-picked copy of the same upstream change.

- Fork invariants verified present after the merge: `NewSafeTransport` still
  resolves the environment proxy (`proxyKindEnv` / `http.ProxyFromEnvironment`);
  the native DoH pool type (`internal/proxy/doh_transport.go`,
  `db.ProxyPool.IsDoH`) is intact; no Petani Proxy code returns; the WorkBuddy
  identity string (`WorkBuddy/5.8.4 workbuddy-ai/5.8.4`) survives and
  `codebuddyCLIVersion` is declared exactly once (`codebuddy_identity.go`); the
  mimo-web (`mw`) provider stays registered with the fork's tool-synthesis
  executor (the fork's `mimo_web.go` / `mimo_web_stream.go` / tests win over
  the branch's older copy); the `disabledModels` gate stays, via the branch's
  two-key `staticAlias` check that supersedes the fork's index mirror; the
  free-pi in-band ad gate stays absent.
- Semantic duplicates left by textual auto-merges were removed:
  `StatusReauthRequired` (providers), the `"hc"` alias key, a duplicated
  `jsontext` import (db), a duplicated `Usage` field + `toolIndexForOutput`
  (executor/stream.go), and a duplicated `setupMediaTestDB` test helper.
  Two `.changes` fragments that arrived with conflict markers baked in
  (904/905, identical halves) were cleaned.
- Verified: `go build ./...` passes (real `web/dist` built via
  `bun web/scripts/web-build.ts`); `go test ./...` passes except three
  DNS-environment failures that reproduce identically on the pre-merge
  baseline in this sandbox (`internal/proxy` hostname-proxy,
  `internal/handlers/media` SSRF target check, and the same class in the new
  `internal/tunnel` package).
