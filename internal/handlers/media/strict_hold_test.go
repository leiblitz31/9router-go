package media

import (
	"net/http"
	"testing"

	internalproxy "9router/proxy/internal/proxy"
)

// A strict pool's client must never be retried direct: answering from the
// host's real IP is exactly what the strict assignment exists to prevent
// (decolua/9router#4696).
func TestMayRetryDirect(t *testing.T) {
	if !mayRetryDirect(nil) {
		t.Error("nil client must allow direct retry")
	}
	lax := &http.Client{Transport: http.DefaultTransport}
	if !mayRetryDirect(lax) {
		t.Error("lax client must allow direct retry")
	}
	strict := internalproxy.ForbidDirectReplay(&http.Client{Transport: http.DefaultTransport})
	if mayRetryDirect(strict) {
		t.Error("strict client must refuse direct retry")
	}
}
