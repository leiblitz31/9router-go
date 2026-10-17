package dashboard

import (
	"io"
	"net/http"

	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/proxygateway"
	json "encoding/json/v2"
)

// ProxyGatewayManager is the global gateway manager instance.
// Initialized at startup.
var ProxyGatewayManager = proxygateway.NewDefaultManager()

// HandleProxyGatewayStatus returns the gateway status.
// GET /api/proxy-gateway/status
func (h *DashboardHandler) HandleProxyGatewayStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := ProxyGatewayManager.Status()
	status.Available = ProxyGatewayManager.IsAvailable()

	handlerutil.WriteJSON(w, http.StatusOK, status)
}

// HandleProxyGatewayToggle enables/disables the gateway.
// POST /api/proxy-gateway/toggle {"enabled": true/false}
func (h *DashboardHandler) HandleProxyGatewayToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	var opErr error
	if req.Enabled {
		opErr = ProxyGatewayManager.Start()
	} else {
		opErr = ProxyGatewayManager.Stop()
	}

	if opErr != nil {
		// If already running/stopped, that's not an error for toggle
		status := ProxyGatewayManager.Status()
		if req.Enabled && status.Running {
			// Already running, fine
		} else if !req.Enabled && !status.Running {
			// Already stopped, fine
		} else {
			handlerutil.WriteJSONError(w, http.StatusInternalServerError, opErr.Error())
			return
		}
	}

	// Sync proxy pool active status with gateway
	h.syncProxyPoolWithGateway(req.Enabled)

	status := ProxyGatewayManager.Status()
	status.Available = ProxyGatewayManager.IsAvailable()

	handlerutil.WriteJSON(w, http.StatusOK, status)
}

// HandleProxyGatewayRestart restarts the gateway.
// POST /api/proxy-gateway/restart
func (h *DashboardHandler) HandleProxyGatewayRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	_ = ProxyGatewayManager.Stop()
	if err := ProxyGatewayManager.Start(); err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	status := ProxyGatewayManager.Status()
	status.Available = ProxyGatewayManager.IsAvailable()

	handlerutil.WriteJSON(w, http.StatusOK, status)
}

// syncProxyPoolWithGateway enables/disables the petani-local-8888 pool
// based on gateway state.
func (h *DashboardHandler) syncProxyPoolWithGateway(enabled bool) {
	if h.Repo == nil {
		return
	}

	// Update the proxy pool's isActive flag via UpdateProxyPool
	// Pool ID: petani-local-8888
	updates := map[string]any{
		"isActive": enabled,
	}
	// UpdateProxyPool expects specific fields; use isActive directly
	// If the pool doesn't exist, this is a no-op
	_ = h.Repo.UpdateProxyPool("petani-local-8888", updates)
}

// HandleProxyGatewayPool returns the current proxy pool (proxies + stats).
// GET /api/proxy-gateway/pool
func (h *DashboardHandler) HandleProxyGatewayPool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pool := ProxyGatewayManager.GetPool()
	if pool == nil {
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"proxies": []any{},
			"count":   0,
			"running": false,
		})
		return
	}

	handlerutil.WriteJSON(w, http.StatusOK, pool)
}

// HandleProxyGatewayHarvest triggers a manual proxy harvest.
// POST /api/proxy-gateway/harvest
// Optional JSON body: {"workers": N} — concurrent feed fetch limit (1-30).
func (h *DashboardHandler) HandleProxyGatewayHarvest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workers := 0
	if r.Body != nil {
		var body struct {
			Workers int `json:"workers"`
		}
		if data, err := io.ReadAll(r.Body); err == nil {
			if err := json.Unmarshal(data, &body); err == nil {
				if body.Workers >= 1 && body.Workers <= 30 {
					workers = body.Workers
				}
			}
		}
	}

	count, err := ProxyGatewayManager.TriggerHarvest(workers)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"proxyCount": count,
	})
}
