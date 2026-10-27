package chat

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"9router/proxy/internal/db"
)

// TestHandleModels_CompatibleNodeDisabledModels pins the claim that the
// disabledModels gate already covers a custom (compatible) provider node, not
// just registry providers.
//
// The dashboard writes the disabled list under the node's storage alias, which
// for a compatible node is the node id itself — GetProviderAlias returns the id
// unchanged because a node id is not in RegistryAliases. So staticAlias,
// outputAlias and providerID all collapse to the same string, and the existing
// two-key check in appendConnectionModels covers it. This test exists so that
// claim is verified rather than assumed: if a future change makes alias
// resolution diverge for nodes, the gate silently stops hiding their models.
func TestHandleModels_CompatibleNodeDisabledModels(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	for _, scope := range []string{"providerConnections", "disabledModels", "customModels"} {
		if scope == "providerConnections" {
			if _, err := database.Exec(`DELETE FROM providerConnections`); err != nil {
				t.Fatalf("clear connections: %v", err)
			}
			continue
		}
		if _, err := database.Exec(`DELETE FROM kv WHERE scope = ?`, scope); err != nil {
			t.Fatalf("clear %s: %v", scope, err)
		}
	}

	const (
		nodeID = "openai-compatible-chat-vsllm"
		prefix = "vsllm"
	)
	if _, err := database.Exec(
		`INSERT INTO providerNodes (id, type, name, data, createdAt, updatedAt) VALUES (?, 'openai-compatible', 'VSLLM', ?, '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`,
		nodeID, `{"prefix":"`+prefix+`","apiType":"openai-compatible","baseUrl":"https://api.vsllm.com/v1"}`,
	); err != nil {
		t.Fatalf("seed providerNode: %v", err)
	}
	for _, id := range []string{"keep-me", "hide-me"} {
		if _, err := database.Exec(
			`INSERT INTO kv (scope, key, value) VALUES ('customModels', ?, ?)`,
			nodeID+"|"+id+"|llm",
			`{"providerAlias":"`+nodeID+`","id":"`+id+`","type":"llm","name":"`+id+`"}`,
		); err != nil {
			t.Fatalf("seed customModels %s: %v", id, err)
		}
	}
	// Keyed by the node id, which is what the dashboard's storageAlias resolves
	// to for a node.
	if _, err := database.Exec(
		`INSERT INTO kv (scope, key, value) VALUES ('disabledModels', ?, ?)`,
		nodeID, `["hide-me"]`,
	); err != nil {
		t.Fatalf("seed disabledModels: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, priority, isActive, data, createdAt, updatedAt) VALUES
		 ('conn-vsllm', ?, 'api-key', 'VSLLM', 1, 1, '{"apiKey":"sk-test"}', '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`,
		nodeID,
	); err != nil {
		t.Fatalf("seed connection: %v", err)
	}

	repo := db.NewRepo(database)
	handler := NewChatHandler(repo)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	handler.HandleModels(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	// A node with no prefix on its connection publishes under the node id
	// itself — GetProviderAlias returns the id unchanged for a node, because a
	// node id is not in RegistryAliases. That is the spelling the disabled
	// list has to match, and the one the dashboard's storageAlias writes.
	if !strings.Contains(body, `"id":"`+nodeID+`/keep-me"`) {
		t.Errorf("%s/keep-me must stay listed, got: %s", nodeID, body)
	}
	if strings.Contains(body, `"id":"`+nodeID+`/hide-me"`) {
		t.Errorf("%s/hide-me must be excluded after being disabled, got: %s", nodeID, body)
	}
}

// TestHandleModels_CompatibleNodeDisabledByPrefix covers the other spelling:
// the same gate must hide the model when the disabled list is keyed by the
// node's prefix rather than its id, because an operator (or an older dashboard
// build) may have written it that way.
func TestHandleModels_CompatibleNodeDisabledByPrefix(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	if _, err := database.Exec(`DELETE FROM providerConnections`); err != nil {
		t.Fatalf("clear connections: %v", err)
	}
	for _, scope := range []string{"disabledModels", "customModels"} {
		if _, err := database.Exec(`DELETE FROM kv WHERE scope = ?`, scope); err != nil {
			t.Fatalf("clear %s: %v", scope, err)
		}
	}

	const (
		nodeID = "openai-compatible-chat-nara"
		prefix = "nara"
	)
	if _, err := database.Exec(
		`INSERT INTO providerNodes (id, type, name, data, createdAt, updatedAt) VALUES (?, 'openai-compatible', 'Nara', ?, '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`,
		nodeID, `{"prefix":"`+prefix+`","apiType":"openai-compatible"}`,
	); err != nil {
		t.Fatalf("seed providerNode: %v", err)
	}
	for _, id := range []string{"keep-me", "hide-me"} {
		if _, err := database.Exec(
			`INSERT INTO kv (scope, key, value) VALUES ('customModels', ?, ?)`,
			nodeID+"|"+id+"|llm",
			`{"providerAlias":"`+nodeID+`","id":"`+id+`","type":"llm","name":"`+id+`"}`,
		); err != nil {
			t.Fatalf("seed customModels %s: %v", id, err)
		}
	}
	if _, err := database.Exec(
		`INSERT INTO kv (scope, key, value) VALUES ('disabledModels', ?, ?)`,
		prefix, `["hide-me"]`,
	); err != nil {
		t.Fatalf("seed disabledModels: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, priority, isActive, data, createdAt, updatedAt) VALUES
		 ('conn-nara', ?, 'api-key', 'Nara', 1, 1, '{"apiKey":"sk-test","prefix":"`+prefix+`"}', '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`,
		nodeID,
	); err != nil {
		t.Fatalf("seed connection: %v", err)
	}

	repo := db.NewRepo(database)
	handler := NewChatHandler(repo)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	handler.HandleModels(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, `"id":"`+prefix+`/keep-me"`) {
		t.Errorf("%s/keep-me must stay listed, got: %s", prefix, body)
	}
	if strings.Contains(body, `"id":"`+prefix+`/hide-me"`) {
		t.Errorf("%s/hide-me must be excluded when disabled by prefix, got: %s", prefix, body)
	}
}
