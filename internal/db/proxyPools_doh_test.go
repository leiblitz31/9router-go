package db

import (
	"testing"
)

// TestGetProxyPool_DoHType verifies a "doh" pool parses like any other pool:
// its urls are resolver specs (doh-mcp aliases or DoH endpoint URLs) rotated
// by NextURL, and the type is never mistaken for an edge relay even though a
// spec can be an https URL on a well-known host.
func TestGetProxyPool_DoHType(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS proxyPools (
		id TEXT PRIMARY KEY,
		data TEXT,
		isActive INTEGER DEFAULT 1,
		testStatus TEXT,
		createdAt TEXT,
		updatedAt TEXT
	);`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	repo := NewRepo(db)

	poolData := `{"name":"doh-resolvers","urls":["cloudflare","google","https://one.one.one.one/dns-query"],"type":"doh","strategy":"round-robin","strictProxy":true}`
	if _, err := db.Exec(`INSERT INTO proxyPools (id, data, isActive) VALUES (?, ?, ?)`, "pool-doh", poolData, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}

	pool, err := repo.GetProxyPool("pool-doh")
	if err != nil {
		t.Fatalf("GetProxyPool failed: %v", err)
	}
	if pool == nil {
		t.Fatal("expected pool, got nil")
	}
	if pool.Type != "doh" {
		t.Errorf("expected Type doh, got %s", pool.Type)
	}
	wantURLs := []string{"cloudflare", "google", "https://one.one.one.one/dns-query"}
	if len(pool.URLs) != len(wantURLs) {
		t.Fatalf("expected URLs %v, got %v", wantURLs, pool.URLs)
	}
	for i := range wantURLs {
		if pool.URLs[i] != wantURLs[i] {
			t.Errorf("URLs[%d] = %q, want %q", i, pool.URLs[i], wantURLs[i])
		}
	}
	if !pool.StrictProxy {
		t.Error("expected StrictProxy true")
	}
	if pool.IsEdgeRelay() {
		t.Error("a doh pool must never read as an edge relay")
	}

	// NextURL rotates across every resolver spec.
	seen := map[string]bool{}
	for i := 0; i < len(wantURLs); i++ {
		seen[pool.NextURL()] = true
	}
	for _, u := range wantURLs {
		if !seen[u] {
			t.Errorf("NextURL rotation never returned %q (saw %v)", u, seen)
		}
	}
}
