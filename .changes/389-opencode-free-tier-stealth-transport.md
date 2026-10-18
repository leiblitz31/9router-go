### feat(opencode): native Bun BoringSSL stealth transport for free tier models

Routes outbound requests to `https://opencode.ai` using a native, pure-Go stealth TLS stack (`bogdanfinn/tls-client` + `fhttp`) matching Bun's BoringSSL fingerprint (17 cipher suites, 12 extensions in baseline order, HTTP/2 pseudo-header order, and non-alphabetical header ordering with W3C trace context).

- **Bun TLS handshake parity:** The official OpenCode CLI compiles as a Bun binary on top of BoringSSL. Upstream's free-tier gate verifies the TLS ClientHello fingerprint (17 cipher suites, 12 extensions) and rejects requests from Go's standard `net/http` or Node.js with `403 FreeTierError`. The new `opencodeStealthClient` reproduces the exact Bun BoringSSL ClientHello spec without cgo.
- **Header ordering & W3C trace context:** Uses `fhttp.HeaderOrderKey` to preserve exact header ordering instead of Go's alphabetical sorting, and stamps standard W3C `traceparent` and `b3` trace headers on outbound requests.
- **Proxy pool propagation:** Recovers connection proxy pool assignments from `reference.Transport` so operator egress proxies still apply and requests do not leak the host IP.
- **Safe fallback:** Automatically uses plain HTTP when dialing local mock servers (`http://`), edge relays (`x-relay-target`), or when `OPENCODE_PLAIN_HTTP=1` is set.
- **Free model routing in Zen:** `forwardZenChat` and `forwardZenResponses` in `internal/proxy/executor/opencode_zen.go` route free-tier models through the stealth transport when targeting the inference gate.

**Verification:**
- Unit tests in `internal/proxy/executor/opencode_stealth_test.go` verifying the 17-cipher/12-extension spec, proxy recovery, W3C trace header formats, and plain HTTP fallback.
- End-to-end streaming test with mock upstream verifying SSE handling through `ForwardOpencode`.
- Full unit test suite passes cleanly across all 46 packages (`go test ./...`).
- Svelte ratchet check (`make vet-svelte`) passes with 0 unresolved identifiers and type debt at baseline.
