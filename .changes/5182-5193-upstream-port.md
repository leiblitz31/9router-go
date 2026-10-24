### Upstream parity: DeepSeek Web userToken wrapper rule (decolua/9router#5182) and OpenCode free-lane no-probe test (decolua/9router#5193)

- DeepSeek Web: a pasted `userToken` in the localStorage object form now only
  counts when its `value` is a string of 20+ chars, in both the executor
  normalizer (`deepSeekUserToken`) and the browser-login reader
  (`DeepSeekUserTokenFromLocalStorage`). A logged-out page holds
  `{"value":null,"__version":"0"}`; the executor previously fell back to
  that raw JSON string as the Bearer token, so the dashboard probe sent a
  credential the identity probe rejects. A wrapper without a usable string
  value now normalizes to "" instead, and the probe reports it as a missing
  credential rather than an invalid one.
- Behavior change, made deliberately: a wrapped value shorter than 20 chars
  no longer validates. The existing `TestValidateDeepSeekWeb` case that
  pinned a 12-char wrapped value as valid was updated to a realistic
  64-char wrapped token, and a new case pins the short-wrapper rejection
  (no upstream probe is issued for it).
- OpenCode free lane: pinned by test that the dashboard Test for the
  `opencode` free lane issues zero upstream probes and reports valid. The
  behavior already existed here (the NoAuth branch returns valid without
  probing, and real free-lane traffic already uses the v2
  `opencode/latest/*` fingerprint); upstream #5193's bug — a Test probe
  sending the rejected v1 `opencode/1.18.31` fingerprint to
  `opencode.ai/zen/v1/models` — never existed in this fork's probe path.
- Investigated and deliberately not ported from #5182: the webpack
  `createRequire` / `import(pathToFileURL)` bundling fixes and the
  playwright-core CJS interop fix have no Go counterpart (this binary is
  not bundled by webpack and launches a system browser over CDP), and the
  PoW-worker file-tracing fix has none either (the PoW hash is pure Go in
  `deepseek_web_pow.go`, not a shipped `.mjs` worker file).

Verified: new table tests for the executor normalizer and the browser-login
reader, the new opencode no-probe test, and full package runs for
`internal/browserlogin`, `internal/proxy/executor`,
`internal/handlers/dashboard`, and `internal/browsercookies`; `gofmt`
clean on all touched files.
