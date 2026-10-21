package db

import (
	"database/sql"
	"testing"
)

const proxyPoolsDDL = `CREATE TABLE IF NOT EXISTS proxyPools (
	id TEXT PRIMARY KEY,
	isActive INTEGER DEFAULT 1,
	testStatus TEXT,
	data TEXT NOT NULL,
	createdAt TEXT NOT NULL,
	updatedAt TEXT NOT NULL
);`

func seedTestRepo(t *testing.T) (*Repo, *sql.DB) {
	t.Helper()
	database, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	if _, err := database.Exec(proxyPoolsDDL); err != nil {
		t.Fatalf("create proxyPools: %v", err)
	}
	return NewRepo(database), database
}

func countPoolsByType(t *testing.T, repo *Repo, poolType string) int {
	t.Helper()
	pools, err := repo.ListProxyPools()
	if err != nil {
		t.Fatalf("ListProxyPools: %v", err)
	}
	count := 0
	for _, p := range pools {
		if p["type"] == poolType {
			count++
		}
	}
	return count
}

// TestSeedDefaultDoHPool covers the seed's decision matrix: plant only when
// no doh pool exists, never touch an operator's own doh pool, stay
// idempotent across repeated boots.
func TestSeedDefaultDoHPool(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T, repo *Repo)
		seedCalls int
		wantSeed  bool // result of the final seed call
		wantDoH   int
		wantTotal int
	}{
		{
			name:      "empty table plants the default pool",
			setup:     func(t *testing.T, repo *Repo) {},
			seedCalls: 1,
			wantSeed:  true,
			wantDoH:   1,
			wantTotal: 1,
		},
		{
			name: "non-doh pool alone does not block the seed",
			setup: func(t *testing.T, repo *Repo) {
				if _, err := repo.InsertProxyPool(ProxyPoolData{
					Name: "relay", ProxyURL: "https://relay.example.com", Type: "vercel",
				}); err != nil {
					t.Fatalf("insert relay pool: %v", err)
				}
			},
			seedCalls: 1,
			wantSeed:  true,
			wantDoH:   1,
			wantTotal: 2,
		},
		{
			name: "existing custom doh pool is left untouched",
			setup: func(t *testing.T, repo *Repo) {
				if _, err := repo.InsertProxyPool(ProxyPoolData{
					Name: "my-doh", URLs: []string{"quad9"}, Type: "doh", StrictProxy: false,
				}); err != nil {
					t.Fatalf("insert custom doh pool: %v", err)
				}
			},
			seedCalls: 1,
			wantSeed:  false,
			wantDoH:   1,
			wantTotal: 1,
		},
		{
			name:      "second boot seeds nothing again",
			setup:     func(t *testing.T, repo *Repo) {},
			seedCalls: 2,
			wantSeed:  false,
			wantDoH:   1,
			wantTotal: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _ := seedTestRepo(t)
			tt.setup(t, repo)

			var seeded bool
			var err error
			for i := 0; i < tt.seedCalls; i++ {
				seeded, err = repo.SeedDefaultDoHPool()
				if err != nil {
					t.Fatalf("SeedDefaultDoHPool call %d: %v", i+1, err)
				}
			}
			if seeded != tt.wantSeed {
				t.Errorf("final seed call = %v, want %v", seeded, tt.wantSeed)
			}
			if got := countPoolsByType(t, repo, "doh"); got != tt.wantDoH {
				t.Errorf("doh pools = %d, want %d", got, tt.wantDoH)
			}
			pools, err := repo.ListProxyPools()
			if err != nil {
				t.Fatalf("ListProxyPools: %v", err)
			}
			if len(pools) != tt.wantTotal {
				t.Errorf("total pools = %d, want %d", len(pools), tt.wantTotal)
			}
		})
	}
}

// TestSeedDefaultDoHPool_Contents verifies the planted row itself: the
// default name, every resolver in order, active, strict, and readable back
// through GetProxyPool as a rotating doh pool.
func TestSeedDefaultDoHPool_Contents(t *testing.T) {
	repo, _ := seedTestRepo(t)

	seeded, err := repo.SeedDefaultDoHPool()
	if err != nil {
		t.Fatalf("SeedDefaultDoHPool: %v", err)
	}
	if !seeded {
		t.Fatal("expected the seed to plant a pool on an empty table")
	}

	pools, err := repo.ListProxyPools()
	if err != nil {
		t.Fatalf("ListProxyPools: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("pools = %d, want 1", len(pools))
	}
	row := pools[0]
	if row["name"] != DefaultDoHPoolName {
		t.Errorf("name = %v, want %q", row["name"], DefaultDoHPoolName)
	}
	if row["isActive"] != true {
		t.Errorf("isActive = %v, want true", row["isActive"])
	}
	if row["strictProxy"] != true {
		t.Errorf("strictProxy = %v, want true", row["strictProxy"])
	}
	urls, ok := row["urls"].([]any)
	if !ok {
		t.Fatalf("urls missing or wrong type: %T", row["urls"])
	}
	if len(urls) != len(DefaultDoHResolvers) {
		t.Fatalf("urls = %d, want %d", len(urls), len(DefaultDoHResolvers))
	}
	for i, want := range DefaultDoHResolvers {
		if urls[i] != want {
			t.Errorf("urls[%d] = %v, want %q", i, urls[i], want)
		}
	}
	if len(DefaultDoHResolvers) != 15 {
		t.Errorf("DefaultDoHResolvers = %d entries, want 15", len(DefaultDoHResolvers))
	}

	id, _ := row["id"].(string)
	pool, err := repo.GetProxyPool(id)
	if err != nil {
		t.Fatalf("GetProxyPool(%q): %v", id, err)
	}
	if !pool.IsDoh() {
		t.Error("seeded pool must read back as a doh pool")
	}
	if pool.Strategy != "round-robin" {
		t.Errorf("Strategy = %q, want round-robin", pool.Strategy)
	}
	if !pool.IsActive {
		t.Error("seeded pool must be active")
	}
}
