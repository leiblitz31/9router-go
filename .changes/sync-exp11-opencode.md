### Sync upstream main (1.9.15-exp.11) + OpenCode free-lane gate fix

- Merge upstream `main` at `9489dca8` (v1.9.15-exp.11), bringing the
  OpenCode free-tier fixes the fork was missing: #677 (free-tier gate body
  fingerprint — the harness preamble plus the twelve genuine CLI tool
  definitions, replacing the old full-gate concealment) and #711 (catalog
  refresh plus the deepseek-v4.1-flash-free context cap at 330k).
- Merge resolutions: `fingerprint.go` / `fingerprint_test.go` taken from
  upstream (the old `FullGateTools` escalation is redundant once #677 sends
  the genuine profile on every attempt; the fork's session-rotation retry in
  `opencode_gate.go` is kept, and both executor closures now re-conceal with
  `ConcealOpenCodeV2FingerprintTools`); `registry_models.go` taken from
  upstream (the cloudflare-ai list shrinks to the upstream subset and cmc
  gains Kimi-K3.1-Pro — routing does not require catalog membership, so
  `cf/@cf/qwen/qwen3.8-27b` and friends still route via the cloudflare-ai
  executor, but the `/v1/models` listing must be re-checked after deploy);
  `capabilities.go` unioned (fork nemotron rows kept, upstream free-id rows
  kept, muse-spark keys follow upstream without the blanket 1M window, and
  both `Jailbreak` and `ThinkingLevels` fields kept on `CapabilitiesDetail`);
  the thinking-levels fixture unioned (fork opencode-go/ocz rows plus
  upstream gemini-web rows, with one missing `},` repaired); the svelte
  compatible-models row taken from upstream (capability icons; the fork's
  Tooltip copy button elsewhere in the file is untouched); `VERSION` at
  1.9.15-exp.11; `combo_test.go`, `retry_after.go` and
  `rate_limit_failover_test.go` taken from upstream (retry cooldown is now
  per-provider: 2h cap, 7d for deepseek-web).
- Local fixes preserved verbatim through the merge: `44fe472b`
  (cloudflare-ai system-message merge) and `b1abfcef` (poolside reasoning
  collision strip). No squash, no drop.
- Node ports assessed, not blindly copied: `483c10f` (normalize colliding
  fingerprint tools in place) is superseded by #677 — the merged
  `ConcealOpenCodeV2FingerprintTools` never renames a tool (its rename map
  is empty by design), so the rename bug class is gone; `1da5b0d` (deepseek
  free 330k cap) is verified present via #711, not duplicated; `ce060c7`
  (TLS ClientHello) is NOT ported — its session-timestamp and body-contract
  gates are already covered in Go (time-based session minting, `<env>`
  preamble), and its TLS gate needs a uTLS-class transport that upstream Go
  has not adopted; the live probe after deploy decides whether it is needed
  at all (Go #677's own probes say the body is the gate regardless of
  transport).
- Test alignment done in the merge's wake (all four were already failing on
  the pre-merge HEAD, verified via worktree): the fork's 120s retry cap test
  updated to the merged per-provider ceiling; the `*gemini*argon*` pattern
  row given the gemini-4 windows it was shadowing; six pre-existing
  thinking-level divergences recorded as Go-intentional pending entries;
  the proxy-pool client test updated off the retired `Transport.Proxy`
  assertion (the proxy now lives in the uTLS dialer plus registry).
- Known remaining red, pre-existing on HEAD and unrelated to this merge:
  `TestStrictPoolTrafficNeverReachesTheProviderDirectly` and
  `TestStrictProxy_ZeroDirectToUpstreamDialOnDeadProxy` (strict-proxy
  direct-dial leak, failing identically before the merge). Left untouched
  on purpose: weakening security tests or hacking the dialer is outside
  this sync's scope; needs its own task.
- Verified: `go test` green for `internal/translator`,
  `internal/providers`, `internal/proxy/executor`; `go vet` clean on all
  four touched packages; `internal/handlers/chat` green except the two
  pre-existing strict-proxy failures above.
