### fix(proxy): report the primary proxy error when the direct fallback is SSRF-blocked

- `FallbackTransport` no longer replaces a primary proxy failure with the
  SSRF refusal of its direct retry: when the base transport fails with a
  proxy error and the direct fallback fails with `ErrSSRFBlocked`, the
  returned error now wraps the primary error and notes the refused fallback.
- In a sandbox whose DNS maps every external host to a sentinel range, the
  direct dial is refused by design, so the old behavior buried the real
  failure (a stalled or refused proxy) behind a guard verdict. Direct
  success, direct non-SSRF failures, and 403-response primaries behave
  exactly as before.
- Local fork fix, not an upstream port. Verified by the new
  `TestFallbackTransportPrimaryErrorOnSSRF` table test (5 cases) and the
  full `internal/proxy` suite (only the known pre-existing
  `TestRequestThroughALoopbackProxyReachesIt` failure remains).
