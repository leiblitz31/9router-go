### OpenCode free lane keeps Bun's ClientHello through HTTP proxies

- Upstream decolua/9router#5297 parity: the TLS-impersonating free-tier fetch now rides an assigned HTTP proxy (proxy pool or per-connection proxy) instead of falling back to the native stack's ClientHello, which the free-tier JA4 gate refuses with 429.
- The DoH transport already presented Bun's hello for the free lane; the HTTP-proxy branch built its transport with the default Chrome spec. `getClientForConnection` now builds a Bun-spec variant (`NewSafeTransportWithProxySpec`) for the `opencode` provider, cached under its own `http\x00bun\x00` key so no other provider shares it.
- Deliberate divergence from Node: Node fails open to a native proxied fetch when its impersonating stack is unavailable; Go keeps its existing fallback machinery (strict pools still refuse the direct replay) — no fallback semantics change in this port.
- Verified by a behavioral test that captures the ClientHello through a local CONNECT proxy: the `opencode` lane sends Bun's 17 ciphers in order, any other provider sends Chrome's list.
