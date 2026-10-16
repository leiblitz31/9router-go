package media

import (
	internalproxy "9router/proxy/internal/proxy"
	"net/http"
)

// mayRetryDirect reports whether a transport-level failure may be retried
// through the direct client. A strict pool's client carries the marker, and
// retrying direct would answer from the host's real IP exactly what the
// strict assignment exists to prevent (decolua/9router#4696).
func mayRetryDirect(client *http.Client) bool {
	return !internalproxy.ForbidsDirectReplay(client)
}
