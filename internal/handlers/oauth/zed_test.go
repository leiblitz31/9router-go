package oauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func zedTestKeypair(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	verifier := zedKeyPrefix + base64.RawURLEncoding.EncodeToString(pemBytes)
	return priv, verifier
}

func zedTestEncrypt(t *testing.T, priv *rsa.PrivateKey, plain string) string {
	t.Helper()
	cipher, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &priv.PublicKey, []byte(plain), nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(cipher)
}

// A callback from an earlier authorize must still decrypt when the dialog
// holds a newer verifier: the ring, not the current verifier, owns the match.
func TestZedDecryptStaleCallback(t *testing.T) {
	zedKeyRingReset()
	oldPriv, _ := zedTestKeypair(t)
	zedKeyRingAdd(oldPriv)
	_, newVerifier := zedTestKeypair(t)
	encrypted := zedTestEncrypt(t, oldPriv, "plain-token-123")
	plain, err := decryptZedToken(encrypted, newVerifier)
	if err != nil {
		t.Fatalf("stale callback should decrypt via ring: %v", err)
	}
	if plain != "plain-token-123" {
		t.Fatalf("wrong plaintext: %q", plain)
	}
}

// A callback encrypted under a key that was never authorized must still fail.
func TestZedDecryptUnknownKey(t *testing.T) {
	zedKeyRingReset()
	_, verifier := zedTestKeypair(t)
	foreign, _ := zedTestKeypair(t)
	encrypted := zedTestEncrypt(t, foreign, "plain-token-123")
	if _, err := decryptZedToken(encrypted, verifier); err == nil {
		t.Fatal("unknown-key callback should fail")
	}
}

// Full handler path: two authorizes, callback from the first, verifier from
// the second — the exact double-tap shape that failed in production.
func TestZedExchangeStaleCallback(t *testing.T) {
	zedKeyRingReset()
	handler := NewOAuthHandler(nil)
	verifiers := make([]string, 0, 2)
	privs := make([]*rsa.PrivateKey, 0, 2)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/api/oauth/zed/authorize", nil)
		rec := httptest.NewRecorder()
		handler.HandleZedAuthorize(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("authorize %d: expected 200, got %d", i, rec.Code)
		}
		body := rec.Body.String()
		verifier := body[strings.Index(body, "zed-rsa-pkcs1:"):]
		verifier = verifier[:strings.Index(verifier, `"`)]
		pemBytes, err := decodeZedVerifier(verifier)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(pemBytes)
		priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		verifiers = append(verifiers, verifier)
		privs = append(privs, priv)
	}
	cipher, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &privs[0].PublicKey, []byte("plain-token-123"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cb := "http://127.0.0.1:58443/?user_id=u1&access_token=" + base64.RawURLEncoding.EncodeToString(cipher)
	req := httptest.NewRequest("POST", "/api/oauth/zed/exchange",
		strings.NewReader(`{"code":"`+cb+`","codeVerifier":"`+verifiers[1]+`"}`))
	rec := httptest.NewRecorder()
	handler.HandleZedExchange(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"authorized"`) {
		t.Errorf("not authorized: %s", rec.Body.String())
	}
}
