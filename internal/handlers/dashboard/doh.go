package dashboard

import (
	"context"
	"net/http"
	"sync"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/proxy"
)

// dohResolverProbeTimeout bounds one per-resolver sweep. Probes run a few
// at a time and each resolver caps its own attempt inside the query budget,
// so the cap only stops a pool naming many dead endpoints from holding the
// request open indefinitely.
const dohResolverProbeTimeout = 45 * time.Second

// dohResolverProbeConcurrency is how many resolvers one sweep probes at
// once: enough that a 15-resolver pool grades in a couple of waves, few
// enough that the sweep does not read as a burst to the resolvers.
const dohResolverProbeConcurrency = 4

// dohResolverProbeResult is one resolver's grade from a per-resolver sweep.
type dohResolverProbeResult struct {
	Spec      string `json:"spec"`
	Endpoint  string `json:"endpoint"`
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// HandleGetDoHDefaults handles GET /api/doh/defaults: the facts the DoH
// page renders that have exactly one owner in the backend — the seeded
// pool's name and resolver list (db.DefaultDoHResolvers) and the alias →
// endpoint table (proxy.DoHAliases). Serving them keeps the frontend from
// carrying a second copy of either list that would drift from what the
// seed plants and what the data plane dials.
func (h *DashboardHandler) HandleGetDoHDefaults(w http.ResponseWriter, r *http.Request) {
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"poolName":  db.DefaultDoHPoolName,
		"resolvers": db.DefaultDoHResolvers,
		"aliases":   proxy.DoHAliases(),
	})
}

// HandleTestProxyPoolResolvers handles
// POST /api/proxy-pools/{id}/test-resolvers: every resolver in a doh pool
// graded on its own, in pool order. The aggregate pool test cannot serve
// this — a doh pool passes it when any one resolver answers, so a dead
// spec hides behind its healthy siblings. Nothing is stored: the stored
// pool verdict still belongs to the aggregate test, and this sweep is a
// diagnostic the operator runs on demand.
func (h *DashboardHandler) HandleTestProxyPoolResolvers(w http.ResponseWriter, r *http.Request) {
	id := getURLParam(r, "id")
	if id == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "missing proxy pool id")
		return
	}

	pool, err := h.Repo.GetProxyPool(id)
	if err != nil || pool == nil {
		handlerutil.WriteJSONError(w, http.StatusNotFound, "proxy pool not found")
		return
	}
	if !pool.IsDoh() {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "pool is not a doh pool")
		return
	}

	resolvers := cleanResolverSpecs(pool.URLs)
	if len(resolvers) == 0 {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "no resolvers configured")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dohResolverProbeTimeout)
	defer cancel()

	results := make([]dohResolverProbeResult, len(resolvers))
	sem := make(chan struct{}, dohResolverProbeConcurrency)
	var wg sync.WaitGroup
	for i, spec := range resolvers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result := dohResolverProbeResult{Spec: spec, Endpoint: proxy.DoHEndpoint(spec)}
			if latencyMs, err := proxy.ProbeDoHResolver(ctx, spec, dohProbeHostname); err != nil {
				result.Error = err.Error()
			} else {
				result.OK = true
				result.LatencyMs = latencyMs
			}
			results[i] = result
		}()
	}
	wg.Wait()

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}
