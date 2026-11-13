### test: keep the unit suite off the network and off the host's DNS

Four unit tests failed on any machine without a direct egress path, and each
failure was the suite asking a real host a question its own fixture had already
answered. None of them was a product bug; all four are fixed by giving the code
under test a seam and pointing the test at a stub, so `go test ./...` passes
offline (AGENTS.md §5.C.3).

- `TestForwardQoder_UnknownModelNamesTheModel` sat for the full 30 s catalogue
  timeout on `api3.qoder.sh` and then failed on the wording of the error. The
  catalogue host is now resolved through `qoderModelListURLFor`, and the package
  `TestMain` aims it at a closed local port alongside the free-pi version
  endpoint. The refresh failure now names the model it was looking for, which is
  what the test was asserting all along: an error that says only "context
  deadline exceeded" tells the caller nothing about which model has no config.
- `TestTTS_EdgeTTS_Success` reached `www.bing.com` for a token and a synthesis
  the httptest stub was already serving. The two Bing URLs are vars with
  `SetBingEndpointsForTesting`, so the test asserts routing rather than the
  network.
- `TestTargetPrivate_RespectsAllowPrivateNetworking` resolved `example.com`
  through the host resolver, so on a host that maps public names into
  198.18.0.0/15 (RFC2544 benchmark space, which `netguard` refuses) the "blocked:
  public name" row failed on the resolver instead of on the predicate.
  `targetPrivate` now resolves through `lookupIP`, and the table supplies the
  answer per row.
- `TestRequestThroughAHostnameProxyReachesIt` asked for `http://upstream.invalid`
  through a fake proxy that only accepted a CONNECT: the fork's uTLS transport
  keeps `Transport.Proxy` nil and tunnels TLS itself, so a plain-http target is
  dialed directly and the SSRF guard refused it. The case now runs against an
  httptest TLS upstream through a real CONNECT proxy, which is the path the
  hostname allowlist exemption exists for — the same shape
  `TestRequestThroughALoopbackProxyReachesIt` already uses.

Verified: `go build ./...`, then `go test` over `./internal/proxy/`,
`./internal/proxy/executor/`, `./internal/handlers/media/`,
`./internal/handlers/chat/` and `./internal/providers/` — all pass with no
egress.

### Two more tests were host-dependent, found by the full `go test ./...`

- `TestX5LargeFile` asserted an absolute 200 ms ceiling on a 64 KB TOML patch.
  TOML's strict `Unmarshal` — the only layer that rejects a duplicate key, which
  `Patch` must run — is quadratic in the keys of one table and costs 360-500 ms
  on the 64 KB sample alone, so the ceiling measured the box rather than the
  patcher (`configpatch_scale_test.go` makes the same argument for the 256 KB
  case). The TOML row now asserts a ratio against that measured floor
  (`x5Ratio`); jsonc and yaml keep the absolute budget, which they meet with
  room to spare.
- `TestDiagnoseHostnameRejectsUnresolvable` asked a real resolver to fail on
  `.invalid`; a resolver that answers every name (this sandbox maps them into
  198.18.0.0/15) turned it into a different finding. `resolveDNS` is now a var
  and the test stubs it.