### OpenCode free-tier gate hardening (opencode-free-bridge recipe)

- `Accept: */*` on every free-tier request (the CLI streams via the body, never
  the header); `proxy.ForwardOpenAI` no longer clobbers it on the opencode free
  lane, which now sends via `proxy.DoRequest` with the full header map.
- W3C trace headers `b3` + `traceparent`, random per request, shaped like the
  CLI's (`BuildOpenCodeHeaders`).
- UA bump `opencode/latest/2.0.29/cli` → `opencode/latest/2.0.32/cli`
  (`DefaultOpenCodeUA` + the `opencode` provider's static headers); pinned by
  `TestDefaultOpenCodeUA_Current` so drift is caught by tests.
- Reactive 403 self-heal: on a `FreeTierError` gate rejection the free lane
  retries once with a fresh session ID and the full 13-tool CLI profile
  (`ConcealOpenCodeV2FullGateTools`), with a 30s process-wide cooldown so an
  IP penalty is never amplified. Applies to `ForwardOpencode` (chat +
  responses) and the free-tier branch of `ForwardOpencodeZen` (chat +
  responses); the keyed lanes are untouched.
- TLS fingerprint spoofing deliberately NOT ported: the sandbox egress proxy
  MITMs TLS, so any ClientHello from this VM terminates at the proxy.

Verified: `go build ./...`, `go test` on the touched packages (only
pre-existing failures remain: `TestDeriveOpencodeSession`,
`TestGetThinkingLevels_MatchesUpstreamFixture`, both failing on clean HEAD).
