### Security: bump Go toolchain and `golang.org/x/*` past the reachable advisories

`govulncheck` reported one reachable advisory and five module-level ones
against the pinned dependency set:

- `GO-2026-6809` (crypto/x509 distrusts compromised intermediates) was
  **reachable** from `internal/mitm`, through `tls.Conn.Write` →
  `x509.Certificate.Verify`. Fixed in `go1.27.3`.
- `GO-2026-6617`, `GO-2026-6612`, `GO-2026-6611`, `GO-2026-6610` and
  `GO-2026-6603` all affect `golang.org/x/net@v0.58.0` (HTTP/2 server HPACK
  race, flow-control refund, CPU exhaustion, malformed framing headers,
  trailer-header memory exhaustion). Fixed in `golang.org/x/net@v0.60.0`.

Changes:

- `go.mod` now pins `toolchain go1.27.3`, `golang.org/x/crypto v0.58.0` and
  `golang.org/x/net v0.61.0`; `golang.org/x/sync`, `golang.org/x/sys` and
  `golang.org/x/text` moved to the versions those two require.
- The `go 1.27` language directive is unchanged, so no source change was
  needed.

Verified:

- `govulncheck ./...` → **0 reachable vulnerabilities** (was 1) and no
  vulnerabilities in imported packages.
- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- All five `CROSS_TARGETS` still build with `CGO_ENABLED=0`, so the single
  portable binary guarantee is intact.
