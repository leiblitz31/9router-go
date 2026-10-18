package chat

import (
	"testing"

	"9router/proxy/internal/db"
)

func TestGetBestConnection_NoAuth_WithProxyStrategy(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS proxyPools (
		id TEXT PRIMARY KEY,
		isActive INTEGER DEFAULT 1,
		testStatus TEXT,
		data TEXT NOT NULL,
		createdAt TEXT NOT NULL,
		updatedAt TEXT NOT NULL
	);`); err != nil {
		t.Fatalf("failed to create proxyPools table: %v", err)
	}

	repo := db.NewRepo(database)

	pool, err := repo.InsertProxyPool(db.ProxyPoolData{
		Name:     "mimo-pool-1",
		ProxyURL: "http://proxy1.example.com:8080",
		Type:     "http",
	})
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}
	poolID := pool["id"].(string)

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY, data TEXT);`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}

	// Save settings with providerStrategies
	settingsJSON := `{
		"providerStrategies": {
			"mimo-free": {
				"proxyPoolId": "` + poolID + `",
				"rotateStrategy": "none"
			}
		}
	}`
	if _, err := database.Exec(`INSERT INTO settings (id, data) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data`, settingsJSON); err != nil {
		t.Fatalf("insert settings: %v", err)
	}

	h := NewChatHandler(repo)

	conn, connData, err := h.GetBestConnection("mimo-free", "", nil, "")
	if err != nil {
		t.Fatalf("GetBestConnection failed: %v", err)
	}

	if conn == nil || connData == nil {
		t.Fatal("expected virtual connection for no-auth provider")
	}

	if connData.ProxyPoolID != poolID {
		t.Errorf("expected ProxyPoolID %s from settings strategy, got %s", poolID, connData.ProxyPoolID)
	}
}

func TestResolveProviderProxyPoolID_CrossAlias(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY, data TEXT);`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}

	// User sets proxyPoolId under "antigravity", request asks for "ag"
	settingsJSON := `{
		"providerStrategies": {
			"antigravity": {
				"proxyPoolId": "pool-antigravity-123"
			}
		}
	}`
	if _, err := database.Exec(`INSERT INTO settings (id, data) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data`, settingsJSON); err != nil {
		t.Fatalf("insert settings: %v", err)
	}

	repo := db.NewRepo(database)
	h := NewChatHandler(repo)

	// 1. Direct match on antigravity
	if got := h.ResolveProviderProxyPoolID("antigravity"); got != "pool-antigravity-123" {
		t.Errorf("expected pool-antigravity-123 for antigravity, got %q", got)
	}

	// 2. Alias match on ag (should find antigravity proxy pool)
	if got := h.ResolveProviderProxyPoolID("ag"); got != "pool-antigravity-123" {
		t.Errorf("expected ag to inherit pool-antigravity-123 from antigravity strategy, got %q", got)
	}

	// 3. Set under "opencode", check "oc"
	settingsJSON2 := `{
		"providerStrategies": {
			"opencode": {
				"proxyPoolId": "pool-opencode-456"
			}
		}
	}`
	if _, err := database.Exec(`UPDATE settings SET data = ? WHERE id = 1`, settingsJSON2); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	if got := h.ResolveProviderProxyPoolID("oc"); got != "pool-opencode-456" {
		t.Errorf("expected oc to inherit pool-opencode-456 from opencode strategy, got %q", got)
	}

	// 4. Cline <-> clinepass alias
	settingsJSON3 := `{
		"providerStrategies": {
			"cline": {
				"proxyPoolId": "pool-cline-789"
			}
		}
	}`
	if _, err := database.Exec(`UPDATE settings SET data = ? WHERE id = 1`, settingsJSON3); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	if got := h.ResolveProviderProxyPoolID("clinepass"); got != "pool-cline-789" {
		t.Errorf("expected clinepass to inherit pool-cline-789 from cline strategy, got %q", got)
	}
}

func TestGetBestConnection_Opencode_InheritsProxyFromStrategy(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY, data TEXT);`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}

	settingsJSON := `{
		"providerStrategies": {
			"opencode": {
				"proxyPoolId": "pool-vercel-relay-xyz"
			}
		}
	}`
	if _, err := database.Exec(`INSERT INTO settings (id, data) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data`, settingsJSON); err != nil {
		t.Fatalf("insert settings: %v", err)
	}

	repo := db.NewRepo(database)
	h := NewChatHandler(repo)

	// Opencode is NoAuth, so GetBestConnection returns a virtual connection
	conn, connData, err := h.GetBestConnection("opencode", "", nil, "")
	if err != nil {
		t.Fatalf("GetBestConnection(opencode) failed: %v", err)
	}
	if conn == nil || connData == nil {
		t.Fatal("expected virtual connection for opencode")
	}
	if connData.ProxyPoolID != "pool-vercel-relay-xyz" {
		t.Errorf("expected ProxyPoolID 'pool-vercel-relay-xyz' on opencode virtual connection, got %q", connData.ProxyPoolID)
	}
}

// Port of upstream decolua/9router#4998 tests/unit/noauth-proxy-fallback.test.js.
// When a proxy pool fails (403/429/5xx), the virtual no-auth connection is
// tagged `proxy:${poolId}` so the failed pool is excluded from rotation on
// retry, instead of surfacing the error immediately.
func TestResolveProviderProxyPoolIDWithExclusions_FiltersExcluded(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS proxyPools (
		id TEXT PRIMARY KEY,
		isActive INTEGER DEFAULT 1,
		testStatus TEXT,
		data TEXT NOT NULL,
		createdAt TEXT NOT NULL,
		updatedAt TEXT NOT NULL
	);`); err != nil {
		t.Fatalf("failed to create proxyPools table: %v", err)
	}

	repo := db.NewRepo(database)

	pool1, err := repo.InsertProxyPool(db.ProxyPoolData{
		Name:     "pool-1",
		ProxyURL: "http://proxy1.example.com:8080",
		Type:     "http",
	})
	if err != nil {
		t.Fatalf("insert pool1: %v", err)
	}
	pool1ID := pool1["id"].(string)

	pool2, err := repo.InsertProxyPool(db.ProxyPoolData{
		Name:     "pool-2",
		ProxyURL: "http://proxy2.example.com:8080",
		Type:     "http",
	})
	if err != nil {
		t.Fatalf("insert pool2: %v", err)
	}
	pool2ID := pool2["id"].(string)

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY, data TEXT);`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}

	// Rotation strategy across both pools
	settingsJSON := `{
		"providerStrategies": {
			"opencode": {
				"rotateStrategy": "round-robin"
			}
		}
	}`
	if _, err := database.Exec(`INSERT INTO settings (id, data) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data`, settingsJSON); err != nil {
		t.Fatalf("insert settings: %v", err)
	}

	h := NewChatHandler(repo)

	// Without exclusions, returns one of the pools
	id := h.ResolveProviderProxyPoolIDWithExclusions("opencode", nil)
	if id != pool1ID && id != pool2ID {
		t.Errorf("expected one of the pool IDs, got %q", id)
	}

	// With pool1 excluded (both bare and tagged forms), returns pool2
	id = h.ResolveProviderProxyPoolIDWithExclusions("opencode", []string{"proxy:" + pool1ID})
	if id != pool2ID {
		t.Errorf("expected pool2 %q after excluding pool1, got %q", pool2ID, id)
	}

	id = h.ResolveProviderProxyPoolIDWithExclusions("opencode", []string{pool1ID})
	if id != pool2ID {
		t.Errorf("expected pool2 %q after excluding pool1 (bare), got %q", pool2ID, id)
	}

	// With both excluded, returns "" (no leak to direct egress)
	id = h.ResolveProviderProxyPoolIDWithExclusions("opencode", []string{"proxy:" + pool1ID, "proxy:" + pool2ID})
	if id != "" {
		t.Errorf("expected empty after excluding all pools, got %q", id)
	}
}

func TestGetBestConnection_NoAuth_TagsProxyConnectionID(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS proxyPools (
		id TEXT PRIMARY KEY,
		isActive INTEGER DEFAULT 1,
		testStatus TEXT,
		data TEXT NOT NULL,
		createdAt TEXT NOT NULL,
		updatedAt TEXT NOT NULL
	);`); err != nil {
		t.Fatalf("failed to create proxyPools table: %v", err)
	}

	repo := db.NewRepo(database)

	pool, err := repo.InsertProxyPool(db.ProxyPoolData{
		Name:     "test-pool",
		ProxyURL: "http://proxy.example.com:8080",
		Type:     "http",
	})
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}
	poolID := pool["id"].(string)

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY, data TEXT);`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}

	settingsJSON := `{
		"providerStrategies": {
			"opencode": {
				"proxyPoolId": "` + poolID + `",
				"rotateStrategy": "none"
			}
		}
	}`
	if _, err := database.Exec(`INSERT INTO settings (id, data) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data`, settingsJSON); err != nil {
		t.Fatalf("insert settings: %v", err)
	}

	h := NewChatHandler(repo)

	conn, _, err := h.GetBestConnection("opencode", "", nil, "")
	if err != nil {
		t.Fatalf("GetBestConnection failed: %v", err)
	}

	// Upstream #4998: connection ID is tagged `proxy:${poolId}`
	expectedID := "proxy:" + poolID
	if conn.ID != expectedID {
		t.Errorf("expected connection ID %q, got %q", expectedID, conn.ID)
	}

	// Connection name uses the pool name
	if conn.Name == nil || *conn.Name != "test-pool" {
		t.Errorf("expected connection name %q, got %v", "test-pool", conn.Name)
	}
}

func TestGetBestConnection_NoAuth_ExcludedPoolReturnsEmpty(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS proxyPools (
		id TEXT PRIMARY KEY,
		isActive INTEGER DEFAULT 1,
		testStatus TEXT,
		data TEXT NOT NULL,
		createdAt TEXT NOT NULL,
		updatedAt TEXT NOT NULL
	);`); err != nil {
		t.Fatalf("failed to create proxyPools table: %v", err)
	}

	repo := db.NewRepo(database)

	pool, err := repo.InsertProxyPool(db.ProxyPoolData{
		Name:     "test-pool",
		ProxyURL: "http://proxy.example.com:8080",
		Type:     "http",
	})
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}
	poolID := pool["id"].(string)

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY, data TEXT);`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}

	settingsJSON := `{
		"providerStrategies": {
			"opencode": {
				"proxyPoolId": "` + poolID + `",
				"rotateStrategy": "none"
			}
		}
	}`
	if _, err := database.Exec(`INSERT INTO settings (id, data) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data`, settingsJSON); err != nil {
		t.Fatalf("insert settings: %v", err)
	}

	h := NewChatHandler(repo)

	// With the single configured pool excluded, the virtual connection falls
	// back to bare "noauth" with no pool (upstream #4998: return null when
	// the configured pool is excluded).
	conn, connData, err := h.GetBestConnection("opencode", "", []string{"proxy:" + poolID}, "")
	if err != nil {
		t.Fatalf("GetBestConnection failed: %v", err)
	}
	if conn.ID != "noauth" {
		t.Errorf("expected bare noauth connection ID, got %q", conn.ID)
	}
	if connData.ProxyPoolID != "" {
		t.Errorf("expected empty ProxyPoolID, got %q", connData.ProxyPoolID)
	}
}
