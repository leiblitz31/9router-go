package dashboard

import (
	"bytes"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"9router/proxy/internal/db"
)

// The DoH page's two backend facts: the defaults endpoint serves the one
// canonical copy of the seeded pool's name, resolver list, and alias table;
// and the per-resolver test grades every spec in a doh pool on its own,
// which the aggregate pool test cannot do (a pool passes it when any one
// resolver answers).

func ensureProxyPoolsTable(t *testing.T, repo *db.Repo) {
	t.Helper()
	if _, err := repo.RawDB().Exec(`CREATE TABLE IF NOT EXISTS proxyPools (
		id TEXT PRIMARY KEY,
		isActive INTEGER DEFAULT 1,
		testStatus TEXT,
		data TEXT NOT NULL,
		createdAt TEXT NOT NULL,
		updatedAt TEXT NOT NULL
	);`); err != nil {
		t.Fatalf("failed to create proxyPools table: %v", err)
	}
}

func TestHandleGetDoHDefaults(t *testing.T) {
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	router := setupTestRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/doh/defaults", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PoolName  string            `json:"poolName"`
		Resolvers []string          `json:"resolvers"`
		Aliases   map[string]string `json:"aliases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to unmarshal defaults: %v", err)
	}
	if body.PoolName != db.DefaultDoHPoolName {
		t.Errorf("poolName = %q, want %q", body.PoolName, db.DefaultDoHPoolName)
	}
	if len(body.Resolvers) != len(db.DefaultDoHResolvers) {
		t.Fatalf("resolvers = %d entries, want %d (the seeded list)", len(body.Resolvers), len(db.DefaultDoHResolvers))
	}
	for i, want := range db.DefaultDoHResolvers {
		if body.Resolvers[i] != want {
			t.Errorf("resolvers[%d] = %q, want %q", i, body.Resolvers[i], want)
		}
	}
	if got := body.Aliases["cloudflare"]; got != "https://cloudflare-dns.com/dns-query" {
		t.Errorf("aliases[cloudflare] = %q, want the cloudflare DoH endpoint", got)
	}
	if got := body.Aliases["google"]; got != "https://dns.google/resolve" {
		t.Errorf("aliases[google] = %q, want the google DoH endpoint", got)
	}
}

func TestHandleTestProxyPoolResolvers(t *testing.T) {
	repo, cleanup := setupTestDB(t)
	defer cleanup()
	ensureProxyPoolsTable(t, repo)
	router := setupTestRouter(repo)

	post := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(nil))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("missing pool is 404", func(t *testing.T) {
		rec := post("/api/proxy-pools/no-such-pool/test-resolvers")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("standard pool is 400", func(t *testing.T) {
		created, err := repo.InsertProxyPool(db.ProxyPoolData{
			Name: "standard", Type: "http", ProxyURL: "http://127.0.0.1:8080",
		})
		if err != nil {
			t.Fatalf("insert standard pool: %v", err)
		}
		rec := post("/api/proxy-pools/" + created["id"].(string) + "/test-resolvers")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("every resolver is graded in pool order", func(t *testing.T) {
		// Both specs point at loopback, which the SSRF-safe query client
		// refuses deterministically — no external network involved, and
		// each resolver must still come back with its own failure rather
		// than the pool failing as a whole.
		specs := []string{"https://127.0.0.1:1/dns-query", "https://[::1]:1/dns-query"}
		created, err := repo.InsertProxyPool(db.ProxyPoolData{
			Name: "doh test", Type: "doh", URLs: specs, StrictProxy: true,
		})
		if err != nil {
			t.Fatalf("insert doh pool: %v", err)
		}
		rec := post("/api/proxy-pools/" + created["id"].(string) + "/test-resolvers")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Results []struct {
				Spec      string `json:"spec"`
				Endpoint  string `json:"endpoint"`
				OK        bool   `json:"ok"`
				LatencyMs int64  `json:"latencyMs"`
				Error     string `json:"error"`
			} `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to unmarshal results: %v", err)
		}
		if len(body.Results) != len(specs) {
			t.Fatalf("results = %d entries, want %d", len(body.Results), len(specs))
		}
		for i, want := range specs {
			got := body.Results[i]
			if got.Spec != want {
				t.Errorf("results[%d].spec = %q, want %q (pool order)", i, got.Spec, want)
			}
			if got.Endpoint != want {
				t.Errorf("results[%d].endpoint = %q, want the spec verbatim %q", i, got.Endpoint, want)
			}
			if got.OK {
				t.Errorf("results[%d].ok = true, want a refused loopback probe to fail", i)
			}
			if got.Error == "" {
				t.Errorf("results[%d].error is empty, want the probe failure", i)
			}
		}
	})
}
