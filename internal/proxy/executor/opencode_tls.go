package executor

import (
	utls "github.com/refraction-networking/utls"

	"9router/proxy/internal/proxy"
)

// opencodeBunCipherSuites is the exact 17-suite BoringSSL/Chrome-family
// cipher list Bun sends (Node open-sse/services/opencodeTlsClient.js
// OPENCODE_CIPHER_LIST, measured 2026-10-26 in Node ce060c7). The
// opencode.ai free-tier gate compares the JA4 cipher-list hash (Bun:
// 5b57614c22b0) and refuses any list that drops an entry — which is why
// the Chrome list the shared uTLS dialer sends by default answers 429
// FreeUsageLimitError while Bun's list answers 200. Keep this verbatim and
// in order.
var opencodeBunCipherSuites = []uint16{
	utls.TLS_AES_128_GCM_SHA256,
	utls.TLS_AES_256_GCM_SHA384,
	utls.TLS_CHACHA20_POLY1305_SHA256,
	utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
	utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
	utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	utls.TLS_RSA_WITH_AES_128_CBC_SHA,
	utls.TLS_RSA_WITH_AES_256_CBC_SHA,
}

// OpencodeBunSpecFactory returns a proxy.ClientHelloSpecFactory that builds
// a fresh ClientHelloSpec per handshake: Chrome's extensions, curves and
// versions with Bun's cipher list swapped in. Only the cipher list changes:
// the gate keys on its JA4 hash, and the rest of the Chrome profile already
// passes. ALPN pinning to http/1.1 is applied by the dialer, not here,
// because the Transport cannot drive HTTP/2 over a non-*tls.Conn.
func OpencodeBunSpecFactory() proxy.ClientHelloSpecFactory {
	return func() (*utls.ClientHelloSpec, error) {
		spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
		if err != nil {
			return nil, err
		}
		ciphers := make([]uint16, len(opencodeBunCipherSuites))
		copy(ciphers, opencodeBunCipherSuites)
		spec.CipherSuites = ciphers
		return &spec, nil
	}
}
