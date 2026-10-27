package executor

import (
	"reflect"
	"testing"

	utls "github.com/refraction-networking/utls"
)

// bunCipherHex renders a cipher list as the JA4-style input: the gate hashes
// this exact sequence, so the test pins order, not just membership.
func bunCipherHex(ciphers []uint16) []uint16 { return ciphers }

func TestOpencodeBunSpecFactory(t *testing.T) {
	factory := OpencodeBunSpecFactory()

	spec, err := factory()
	if err != nil {
		t.Fatalf("OpencodeBunSpecFactory() error = %v", err)
	}
	if len(spec.CipherSuites) != 17 {
		t.Fatalf("cipher count = %d, want 17 (Bun's list)", len(spec.CipherSuites))
	}
	want := []uint16{
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
	if !reflect.DeepEqual(bunCipherHex(spec.CipherSuites), want) {
		t.Errorf("cipher list = %v, want Bun order %v", spec.CipherSuites, want)
	}

	// The list must actually differ from Chrome's: otherwise the swap is a
	// no-op and the gate would keep answering 429.
	chrome, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		t.Fatalf("UTLSIdToSpec(Chrome) error = %v", err)
	}
	if reflect.DeepEqual(spec.CipherSuites, chrome.CipherSuites) {
		t.Errorf("Bun cipher list equals Chrome's; the swap would change nothing")
	}

	// Freshness: two factory calls must not share the cipher slice, so
	// concurrent handshakes cannot race on one spec.
	second, err := factory()
	if err != nil {
		t.Fatalf("second factory() error = %v", err)
	}
	second.CipherSuites[0] = 0x0000
	if spec.CipherSuites[0] == 0x0000 {
		t.Errorf("factory calls share the cipher slice; specs must be independent")
	}
	if len(spec.Extensions) == 0 {
		t.Errorf("spec has no extensions; expected Chrome's extension set")
	}
}
