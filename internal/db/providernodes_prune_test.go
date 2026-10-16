package db

import (
	"database/sql"
	"os"
	"testing"
)

// setupPruneTestDB mirrors setupTestDB's schema shape for the two tables this
// test touches (providerNodes, kv), without dragging in the rest of the suite's
// fixture.
func setupPruneTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "test_nodeprune_*.sqlite")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	tmpFile.Close()

	database, err := OpenDatabase(tmpFile.Name())
	if err != nil {
		os.Remove(tmpFile.Name())
		t.Fatalf("OpenDatabase: %v", err)
	}
	cleanup := func() {
		database.Close()
		os.Remove(tmpFile.Name())
	}

	for _, stmt := range []string{
		`CREATE TABLE providerNodes (
			id TEXT PRIMARY KEY,
			type TEXT,
			name TEXT,
			data TEXT,
			createdAt TEXT,
			updatedAt TEXT
		)`,
		`CREATE TABLE providerConnections (
			id TEXT PRIMARY KEY,
			provider TEXT,
			data TEXT
		)`,
		`CREATE TABLE kv (scope TEXT NOT NULL, key TEXT NOT NULL, value TEXT, PRIMARY KEY (scope, key))`,
	} {
		if _, err := database.Exec(stmt); err != nil {
			cleanup()
			t.Fatalf("schema %s: %v", stmt, err)
		}
	}
	return database, cleanup
}

func TestDeleteProviderNode_PrunesCustomModels(t *testing.T) {
	database, cleanup := setupPruneTestDB(t)
	defer cleanup()
	repo := NewRepo(database)

	node := `{"name":"Local Llama","baseUrl":"http://127.0.0.1:11434/v1"}`
	if _, err := database.Exec(
		`INSERT INTO providerNodes (id, type, name, data, createdAt, updatedAt) VALUES (?, ?, ?, ?, ?, ?)`,
		"openai-compatible-chat-x", "custom", "Local Llama", node, "2026-07-18T00:00:00Z", "2026-07-18T00:00:00Z",
	); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	// Rows the deleted node owns, rows that belong to a different node whose id
	// merely shares a prefix, and a row in another scope.
	rows := []struct{ scope, key string }{
		{"customModels", "openai-compatible-chat-x|llama3|llm"},
		{"customModels", "openai-compatible-chat-x|img|img2txt"},
		{"customModels", "openai-compatible-chat-xy|other|llm"},
		{"customModels", "openai-compatible-chat-x2|third|llm"},
		{"modelAliases", "openai-compatible-chat-x|llama3"},
	}
	for _, r := range rows {
		if _, err := database.Exec(`INSERT INTO kv (scope, key, value) VALUES (?, ?, ?)`, r.scope, r.key, "{}"); err != nil {
			t.Fatalf("seed kv %s/%s: %v", r.scope, r.key, err)
		}
	}

	if err := repo.DeleteProviderNode("openai-compatible-chat-x"); err != nil {
		t.Fatalf("DeleteProviderNode: %v", err)
	}

	remaining, err := database.Query(`SELECT scope, key FROM kv ORDER BY scope, key`)
	if err != nil {
		t.Fatalf("read kv: %v", err)
	}
	defer remaining.Close()
	var got []string
	for remaining.Next() {
		var scope, key string
		if err := remaining.Scan(&scope, &key); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, scope+"/"+key)
	}
	want := []string{
		"customModels/openai-compatible-chat-x2|third|llm",
		"customModels/openai-compatible-chat-xy|other|llm",
		"modelAliases/openai-compatible-chat-x|llama3",
	}
	if len(got) != len(want) {
		t.Fatalf("kv rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
	}

	var nodeCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM providerNodes WHERE id = ?`, "openai-compatible-chat-x").Scan(&nodeCount); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodeCount != 0 {
		t.Errorf("node row survived deletion (count=%d)", nodeCount)
	}
}

func TestEscapeLikePrefix(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"a%b", `a\%b`},
		{"a_b", `a\_b`},
		{`a\b`, `a\\b`},
	} {
		if got := escapeLikePrefix(tc.in); got != tc.want {
			t.Errorf("escapeLikePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
