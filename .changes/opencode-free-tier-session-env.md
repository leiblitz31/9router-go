### Fix OpenCode free-tier 429s from non-OpenCode clients (session clock + CLI env marker)

- `TranslateOpenCodeSessionID` now mints translated sessions with the current
  clock (`time.Now()`) instead of a SHA-256-digest-derived timestamp, and
  memoizes them per (clientTool, sessionId) with a 2h TTL and a 5000-entry
  cap (upstream decolua/9router#5222, issue #5218). Upstream decodes the
  12-hex session prefix and rejects future or ~60d-stale sessions with 429
  `FreeUsageLimitError`; a digest-derived clock is uniform over 2^36 ms, so
  most translated ids were rejected — while clients that sent no session
  (stable per-connection id minted with `time.Now()`) passed. Note the
  correction to the Hermes story: the router's `ExtractSessionID` does
  NOT read `x-opencode-session` (it reads x-claude-code-session-id,
  x-session-id, session-id, session_id, x-amp-thread-id), so Hermes's
  `x-opencode-session` affinity header never reaches this translator —
  Hermes traffic uses the stable per-connection session, and the session
  fix benefits clients that send a recognized session header (e.g. Claude
  Code). For Hermes itself, the operative change in this commit is the
  `<env>` injection below. Canonical `ses_` ids still pass through
  verbatim; the counter stays pinned to 1.
- The oc free lane (`ForwardOpencode`, `forwardOpencodeGoFree`, and the
  muse-spark Responses path) now injects the CLI `<env>` block (Working
  directory + Workspace root folder, both `/workspace`) into the effective
  system prompt, idempotently: a complete block is left alone, a partial
  block is completed in place, and no second block is ever added
  (upstream #5218 fix #1, #5222). Covers OpenAI messages[], Claude
  top-level system, and Responses instructions.
- Shell wording assessed, not changed: the free lane already sends the full
  genuine CLI tool profile on the first attempt (#677), so no equivalent gap
  was found.
- Scope note: the session translator is shared code, so the keyed
  opencode-zen/opencode-go lanes inherit the current-clock translation for
  non-canonical client sessions. A current-clock canonical id is valid on
  every lane; keyed lanes without a client session already used
  `GenerateOpenCodeSessionID` and are unaffected. The env marker is oc
  free-lane only and never touches zen/go.
- Verified: new table-driven regression tests (prefix-timestamp decode
  within the mint window modulo 2^36, memo stability and isolation,
  canonical passthrough, TTL expiry, env-marker idempotency and
  partial-block completion); full `internal/proxy/...` suite green except
  the pre-existing `TestRequestThroughALoopbackProxyReachesIt` failure
  (strict-proxy leak, separate task, untouched).
