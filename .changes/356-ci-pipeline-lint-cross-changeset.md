### ci: gate the diff with lint, vulnerability, guard, and cross-compile jobs

The pipeline enforced `go test`, `-race`, integration, Windows service tests,
Docker build, and a PGO assertion, but nothing checked formatting, static
analysis, reachable CVEs, the frontend linter, or the workflow files themselves —
and `make cross`, which every release runs, executed only on a tag.

Four jobs are added to `.github/workflows/ci.yml`, plus one omission is closed:

- **`lint`** — `golangci-lint` (errcheck, govet, ineffassign, staticcheck,
  unused, bodyclose, noctx, rowserrcheck, sqlclosecheck, errorlint, gofmt),
  `govulncheck`, `oxlint --deny-warnings`, and `actionlint`. It builds no SPA, so
  it reports a formatting slip or a reachable CVE in about a minute. The
  golangci-lint step runs with `--new-from-rev` against the PR base (or `HEAD~1`
  on a push): the tree carries 141 pre-existing findings and 147 files `gofmt`
  would rewrite, so a whole-tree gate would fail on its first PR and be disabled
  by its second. This is the same ratchet shape as `make vet-svelte`.
- **`cross`** — runs `make cross`, the exact release command, then asserts with
  `file` that each of the five artifacts is the architecture its asset name
  claims, and that `SHA256SUMS.txt` covers them and verifies. The updater picks a
  download by matching OS and arch substrings in the asset name, so a
  mislabelled arm64 binary would install and then fail to exec.
- **`changeset`** — fails a PR with no `.changes/` fragment, and fails harder if
  `CHANGELOG.md` is touched (it is written only by `make changelog-merge` at
  release time). Of the last forty merged PRs, five carry no fragment and only
  three of those are the `chore: bump` and `[ImgBot]` commits the rule exempts —
  two entries were never written. The check exempts exactly those two titles and
  documentation-only diffs.
- **concurrency** — one run per ref, `cancel-in-progress`, so a force-push does
  not leave a superseded run's check overwriting the current one.

The two `run:` scripts `release.yml` had never been linted, because `actionlint`
was not installed on any runner. It now is (via `go run`), and it reported two
shellcheck findings in the changelog-fallback block (`SC2010` on `ls | grep`,
`SC2046` on an unquoted `cat $(...)`). That block is rewritten with a glob and a
quoted read loop; its output was verified byte-identical against this repo's
fragments before the change.

`govulncheck` immediately found a reachable advisory — **GO-2026-5320**, a
cross-site-scripting issue in `github.com/yuin/goldmark` v1.7.8 reachable from
`tools/sitegen`, which no other gate examines. `goldmark` is bumped to v1.7.17,
which is not affected.

Three `oxlint` warnings are fixed rather than suppressed (an unused import, an
unused destructured parameter, and an unnecessary escape), so the new
`--deny-warnings` gate starts from zero.

Verified locally: `actionlint` on all three workflows with shellcheck present,
`golangci-lint --new-from-rev` (0 issues on this diff, and a planted violation is
reported), `govulncheck` (no vulnerabilities), `oxlint --deny-warnings` (clean),
`bun test src` (344 pass), `make cross` plus the artifact and checksum
assertions (including negative controls: a corrupted binary fails `sha256sum -c`
and a mislabelled architecture fails the `file` check), and the `changeset`
guard against seven git states — code change with and without a fragment,
`CHANGELOG.md` touched, a `chore:` bump, a docs-only diff, an `[ImgBot]` diff,
and a fragment-only edit.

GoReleaser and a `ghcr.io` publish were considered and not adopted: `install.sh`,
`install.ps1`, and `internal/updater` all download a bare binary named
`9router-go-<os>-<arch>`, while GoReleaser's default `.tar.gz`/`.zip` archives
would break all three, and `release.yml` already produces the binaries, checksums,
and multi-arch Docker Hub image on a tag.
