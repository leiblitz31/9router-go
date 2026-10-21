package dashboard

import (
	"bytes"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Doh pools are created, updated, and tested through their resolver specs:
// create must persist type+urls verbatim (the urls used to be dropped on
// the floor before reaching the stored pool data), and the test endpoint
// must probe DNS through the pool's own resolvers instead of dialing a
// spec like "cloudflare" as an HTTP proxy.
func TestProxyPoolDoH(t *testing.T) {
	repo, cleanup := setupTestDB(t)
	defer cleanup()

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

	router := setupTestRouter(repo)

	post := func(path string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		bodyBytes, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Create: blank specs are cleaned, the rest stored in order.
	rec := post("/api/proxy-pools", map[string]any{
		"name": "DoH Resolvers",
		"type": "doh",
		"urls": []string{"cloudflare", " https://one.one.one.one/dns-query ", ""},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create doh pool expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("failed to unmarshal created pool: %v", err)
	}
	poolID, _ := created["id"].(string)
	if poolID == "" {
		t.Fatalf("created pool has no id: %v", created)
	}
	if created["type"] != "doh" {
		t.Errorf("created type = %v, want doh", created["type"])
	}
	wantURLs := []string{"cloudflare", "https://one.one.one.one/dns-query"}
	gotURLs, _ := created["urls"].([]any)
	if len(gotURLs) != len(wantURLs) {
		t.Fatalf("created urls = %v, want %v", created["urls"], wantURLs)
	}
	for i := range wantURLs {
		if gotURLs[i] != wantURLs[i] {
			t.Errorf("created urls[%d] = %v, want %q", i, gotURLs[i], wantURLs[i])
		}
	}

	// The stored pool reads back as a doh pool with the same specs.
	stored, err := repo.GetProxyPool(poolID)
	if err != nil {
		t.Fatalf("GetProxyPool failed: %v", err)
	}
	if !stored.IsDoh() {
		t.Errorf("stored pool type = %q, want doh", stored.Type)
	}
	if len(stored.URLs) != len(wantURLs) {
		t.Fatalf("stored urls = %v, want %v", stored.URLs, wantURLs)
	}
	for i := range wantURLs {
		if stored.URLs[i] != wantURLs[i] {
			t.Errorf("stored urls[%d] = %q, want %q", i, stored.URLs[i], wantURLs[i])
		}
	}

	// Create without a usable resolver is refused before anything is stored.
	for _, urls := range [][]string{{}, {"  "}} {
		rec := post("/api/proxy-pools", map[string]any{
			"name": "Empty DoH",
			"type": "doh",
			"urls": urls,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("create doh pool with urls %v expected 400, got %d: %s", urls, rec.Code, rec.Body.String())
		}
	}

	// Update: swapping the resolver list round-trips intact.
	putBody, _ := json.Marshal(map[string]any{
		"type": "doh",
		"urls": []string{"google"},
	})
	putReq := httptest.NewRequest(http.MethodPut, "/api/proxy-pools/"+poolID, bytes.NewReader(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	router.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("update doh pool expected 200, got %d: %s", putRec.Code, putRec.Body.String())
	}
	// GetProxyPool serves a process-wide cache that UpdateProxyPool drops,
	// so this read reflects the stored row.
	updated, err := repo.GetProxyPool(poolID)
	if err != nil {
		t.Fatalf("GetProxyPool after update failed: %v", err)
	}
	if len(updated.URLs) != 1 || updated.URLs[0] != "google" {
		t.Errorf("updated urls = %v, want [google]", updated.URLs)
	}
	if !updated.IsDoh() {
		t.Errorf("updated pool type = %q, want doh", updated.Type)
	}

	// Test: a pool whose only resolver is unreachable fails the probe, and
	// the failure is persisted as the pool's test status.
	rec = post("/api/proxy-pools", map[string]any{
		"name": "Dead DoH",
		"type": "doh",
		"urls": []string{"http://127.0.0.1:1/dns-query"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create dead doh pool expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var dead map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &dead); err != nil {
		t.Fatalf("failed to unmarshal dead pool: %v", err)
	}
	deadID, _ := dead["id"].(string)

	testRec := post("/api/proxy-pools/"+deadID+"/test", nil)
	if testRec.Code != http.StatusOK {
		t.Fatalf("test doh pool expected 200, got %d: %s", testRec.Code, testRec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(testRec.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal test result: %v", err)
	}
	if result["success"] != false {
		t.Fatalf("expected success=false for a dead resolver, got %v", result)
	}
	if errStr, _ := result["error"].(string); errStr == "" {
		t.Fatalf("expected an error message for a dead resolver, got %v", result)
	}

	pools, err := repo.ListProxyPools()
	if err != nil {
		t.Fatalf("ListProxyPools failed: %v", err)
	}
	for _, p := range pools {
		if p["id"] == deadID && p["testStatus"] != "failed" {
			t.Errorf("dead pool testStatus = %v, want failed", p["testStatus"])
		}
	}

	// Test: a doh pool with no resolvers at all (seeded directly, since
	// create refuses one) fails with an explanation instead of a dial.
	if _, err := repo.RawDB().Exec(
		`INSERT INTO proxyPools (id, isActive, testStatus, data, createdAt, updatedAt) VALUES (?, 1, 'unknown', ?, '', '')`,
		"pool-doh-empty", `{"name":"Empty DoH","type":"doh","urls":[]}`,
	); err != nil {
		t.Fatalf("seed empty doh pool: %v", err)
	}
	testRec = post("/api/proxy-pools/pool-doh-empty/test", nil)
	var emptyResult map[string]any
	if err := json.Unmarshal(testRec.Body.Bytes(), &emptyResult); err != nil {
		t.Fatalf("failed to unmarshal empty-pool test result: %v", err)
	}
	if emptyResult["success"] != false {
		t.Fatalf("expected success=false for a resolver-less pool, got %v", emptyResult)
	}
}
