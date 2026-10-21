package dashboard

import (
	"context"
	"net/http"
	"strings"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/proxy"
)

// dohProbeHostname is the name a doh pool test resolves through the pool's
// own resolvers. It is a stable, anycasted name every public resolver
// answers, so a failure indicts the resolver rather than the name.
const dohProbeHostname = "cloudflare.com"

// dohProbeTimeout bounds one pool test. The resolver already fails over
// across the pool's endpoints with per-endpoint timeouts, so the cap only
// stops a many-resolver pool from holding the request open indefinitely.
const dohProbeTimeout = 20 * time.Second

// cleanResolverSpecs trims resolver specs and drops blanks, preserving
// order. Create and test both run it so a spec list means the same thing
// stored as it does probed.
func cleanResolverSpecs(specs []string) []string {
	var out []string
	for _, s := range specs {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// testDoHPool tests a doh pool by resolving a well-known hostname through
// the pool's own resolvers. The pool passes when at least one resolver
// answers — the same failover the data plane relies on (see
// proxy.ProbeDoHResolvers) — and the verdict is stored through
// writeProbeResult like any other pool's, so the row repaints with a real
// measured latency.
func (h *DashboardHandler) testDoHPool(w http.ResponseWriter, r *http.Request, id string, pool *db.ProxyPool) {
	resolvers := cleanResolverSpecs(pool.URLs)
	if len(resolvers) == 0 {
		h.writeProbeResult(w, id, false, 0, "no resolvers configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dohProbeTimeout)
	defer cancel()
	latencyMs, err := proxy.ProbeDoHResolvers(ctx, resolvers, dohProbeHostname)
	if err != nil {
		h.writeProbeResult(w, id, false, 0, err.Error())
		return
	}
	h.writeProbeResult(w, id, true, latencyMs, "")
}
