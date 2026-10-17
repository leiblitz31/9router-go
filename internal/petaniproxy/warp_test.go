package petaniproxy

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestWarpKeypair(t *testing.T) {
	privB64, pubB64, err := GenerateWarpX25519Keypair()
	if err != nil {
		t.Fatalf("GenerateWarpX25519Keypair() error = %v", err)
	}
	priv, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil || len(priv) != 32 {
		t.Fatalf("private key must decode to 32 bytes, got len=%d err=%v", len(priv), err)
	}
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != 32 {
		t.Fatalf("public key must decode to 32 bytes, got len=%d err=%v", len(pub), err)
	}
	if privB64 == pubB64 {
		t.Fatal("private and public keys must differ")
	}
	priv2, pub2, err := GenerateWarpX25519Keypair()
	if err != nil {
		t.Fatalf("GenerateWarpX25519Keypair() second call error = %v", err)
	}
	if priv2 == privB64 || pub2 == pubB64 {
		t.Fatal("successive keypairs must differ (not deterministic)")
	}
}

// warpTestServer returns an httptest server answering the reg endpoint with payload.
func warpTestServer(t *testing.T, payload string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("User-Agent"); got != WarpUserAgent {
			t.Errorf("User-Agent = %q, want %q", got, WarpUserAgent)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json; charset=UTF-8" {
			t.Errorf("Content-Type = %q", got)
		}
		var req warpRegRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("request body is not valid JSON: %v", err)
		}
		if req.Model != "PC" || req.Locale != "en_US" {
			t.Errorf("request payload = %+v, want model=PC locale=en_US", req)
		}
		if req.Key == "" || req.TOS == "" {
			t.Errorf("request missing key/tos: %+v", req)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWarpRegisterAccount(t *testing.T) {
	full := `{"id":"abc-123","token":"tok","config":{"peers":[{"public_key":"PEERKEY","endpoint":{"host":"203.0.113.7:2408"}}],"interface":{"addresses":{"v4":"172.16.5.6/32","v6":"2606:4700:110:abc::1/128"}}}}`
	tests := []struct {
		name       string
		payload    string
		wantPeer   string
		wantEP     string
		wantV4     string
		wantV6     string
		wantAcctID string
		wantToken  string
	}{
		{
			name:       "full response parsed",
			payload:    full,
			wantPeer:   "PEERKEY",
			wantEP:     "203.0.113.7:2408",
			wantV4:     "172.16.5.6/32",
			wantV6:     "2606:4700:110:abc::1/128",
			wantAcctID: "abc-123",
			wantToken:  "tok",
		},
		{
			name:       "empty peers fall back to defaults",
			payload:    `{"id":"x","token":"y","config":{"peers":[],"interface":{"addresses":{"v4":"","v6":""}}}}`,
			wantPeer:   defaultWarpPeerPublicKey,
			wantEP:     defaultWarpEndpoint,
			wantV4:     defaultWarpV4Address,
			wantV6:     "",
			wantAcctID: "x",
			wantToken:  "y",
		},
		{
			name:     "missing config falls back to defaults",
			payload:  `{}`,
			wantPeer: defaultWarpPeerPublicKey,
			wantEP:   defaultWarpEndpoint,
			wantV4:   defaultWarpV4Address,
			wantV6:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := warpTestServer(t, tt.payload, http.StatusOK)
			got, err := RegisterWarpAccountToURL(context.Background(), srv.Client(), srv.URL)
			if err != nil {
				t.Fatalf("RegisterWarpAccountToURL() error = %v", err)
			}
			if got.PeerPublicKey != tt.wantPeer {
				t.Errorf("PeerPublicKey = %q, want %q", got.PeerPublicKey, tt.wantPeer)
			}
			if got.Endpoint != tt.wantEP {
				t.Errorf("Endpoint = %q, want %q", got.Endpoint, tt.wantEP)
			}
			if got.V4Address != tt.wantV4 {
				t.Errorf("V4Address = %q, want %q", got.V4Address, tt.wantV4)
			}
			if got.V6Address != tt.wantV6 {
				t.Errorf("V6Address = %q, want %q", got.V6Address, tt.wantV6)
			}
			if got.AccountID != tt.wantAcctID || got.Token != tt.wantToken {
				t.Errorf("AccountID/Token = %q/%q, want %q/%q", got.AccountID, got.Token, tt.wantAcctID, tt.wantToken)
			}
			if got.DNS != DefaultWarpDNS {
				t.Errorf("DNS = %q, want %q", got.DNS, DefaultWarpDNS)
			}
			if got.PrivateKey == "" || got.PublicKey == "" {
				t.Error("generated keypair must be embedded in the profile")
			}
			if got.CreatedAt == "" {
				t.Error("CreatedAt must be set")
			}
		})
	}
}

func TestWarpRegisterAccountErrors(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		status  int
	}{
		{name: "server error", payload: `boom`, status: http.StatusInternalServerError},
		{name: "bad json", payload: `{not json`, status: http.StatusOK},
		{name: "created is accepted", payload: `{"id":"a"}`, status: http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := warpTestServer(t, tt.payload, tt.status)
			got, err := RegisterWarpAccountToURL(context.Background(), srv.Client(), srv.URL)
			if tt.name == "created is accepted" {
				if err != nil {
					t.Fatalf("201 must be accepted, got error %v", err)
				}
				if got == nil || got.AccountID != "a" {
					t.Fatalf("unexpected profile %+v", got)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if got != nil {
				t.Errorf("expected nil profile on error, got %+v", got)
			}
		})
	}
}

func TestWarpWireguardConf(t *testing.T) {
	tests := []struct {
		name    string
		profile *WarpProfile
		want    string
	}{
		{
			name: "v4 and v6 addresses",
			profile: &WarpProfile{
				PrivateKey: "PRIV", PublicKey: "PUB", PeerPublicKey: "PEER",
				Endpoint: "203.0.113.7:2408", V4Address: "172.16.5.6/32",
				V6Address: "2606:4700:110:abc::1/128", DNS: "1.1.1.1, 1.0.0.1",
			},
			want: `[Interface]
PrivateKey = PRIV
Address = 172.16.5.6/32, 2606:4700:110:abc::1/128
DNS = 1.1.1.1, 1.0.0.1

[Peer]
PublicKey = PEER
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = 203.0.113.7:2408
`,
		},
		{
			name: "v6 omitted, default dns",
			profile: &WarpProfile{
				PrivateKey: "PRIV", PeerPublicKey: "PEER",
				Endpoint: "162.159.193.10:2408", V4Address: "172.16.0.2/32", DNS: "",
			},
			want: `[Interface]
PrivateKey = PRIV
Address = 172.16.0.2/32
DNS = 1.1.1.1

[Peer]
PublicKey = PEER
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = 162.159.193.10:2408
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildWarpWireguardConf(tt.profile); got != tt.want {
				t.Errorf("BuildWarpWireguardConf() diff:\n got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestWarpSingboxConfig(t *testing.T) {
	profile := &WarpProfile{
		PrivateKey: "PRIV", PeerPublicKey: "PEER",
		Endpoint: "203.0.113.7:2408", V4Address: "172.16.5.6/32",
		V6Address: "2606:4700:110:abc::1/128",
	}
	cfg, err := BuildWarpSingboxConfig(profile, 10808)
	if err != nil {
		t.Fatalf("BuildWarpSingboxConfig() error = %v", err)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("log.level = %q, want warn", cfg.Log.Level)
	}
	if len(cfg.Inbounds) != 1 || cfg.Inbounds[0].Type != "mixed" || cfg.Inbounds[0].ListenPort != 10808 {
		t.Errorf("unexpected inbounds: %+v", cfg.Inbounds)
	}
	ob := cfg.Outbounds[0]
	if ob.Type != "wireguard" || ob.Tag != "warp-out" || ob.Server != "203.0.113.7" || ob.ServerPort != 2408 {
		t.Errorf("unexpected outbound: %+v", ob)
	}
	if len(ob.LocalAddress) != 2 || ob.LocalAddress[0] != "172.16.5.6/32" {
		t.Errorf("unexpected local_address: %v", ob.LocalAddress)
	}
	if ob.PrivateKey != "PRIV" || ob.PeerPublicKey != "PEER" {
		t.Errorf("unexpected keys: %+v", ob)
	}
	for _, r := range ob.Reserved {
		if r != 0 {
			t.Errorf("reserved must be [0 0 0], got %v", ob.Reserved)
		}
	}
	// Round-trips through JSON with the expected key names.
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal error = %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	obMap := m["outbounds"].([]any)[0].(map[string]any)
	for _, key := range []string{"server", "server_port", "local_address", "private_key", "peer_public_key", "reserved"} {
		if _, ok := obMap[key]; !ok {
			t.Errorf("sing-box JSON missing key %q", key)
		}
	}

	// Malformed endpoints must error (Python would crash unpacking split()).
	for _, ep := range []string{"noport", "10.0.0.1:notaport"} {
		bad := *profile
		bad.Endpoint = ep
		if _, err := BuildWarpSingboxConfig(&bad, 10808); err == nil {
			t.Errorf("BuildWarpSingboxConfig(%q) expected error, got nil", ep)
		}
	}
}

func TestWarpFindRouterDB(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "data.sqlite")
	if err := os.WriteFile(fake, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("env var wins", func(t *testing.T) {
		t.Setenv("ROUTER_DB_PATH", fake)
		if got := FindWarpRouterDB(dir); got != fake {
			t.Errorf("FindWarpRouterDB() = %q, want %q", got, fake)
		}
	})
	t.Run("missing env file falls through", func(t *testing.T) {
		t.Setenv("ROUTER_DB_PATH", filepath.Join(dir, "nope.sqlite"))
		if got := FindWarpRouterDB(dir); got != "" {
			t.Errorf("FindWarpRouterDB() = %q, want empty", got)
		}
	})
	t.Run("sibling candidate found", func(t *testing.T) {
		t.Setenv("ROUTER_DB_PATH", "")
		t.Setenv("BANSOS_ROUTER_DB", "")
		t.Setenv("NINEROUTER_DB", "")
		// baseDir/<up>/9router/data/db/data.sqlite
		up := filepath.Join(dir, "petani-proxy")
		cand := filepath.Join(dir, "9router", "data", "db", "data.sqlite")
		if err := os.MkdirAll(filepath.Dir(cand), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cand, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := FindWarpRouterDB(up); got != cand {
			t.Errorf("FindWarpRouterDB() = %q, want %q", got, cand)
		}
	})
	t.Run("nothing found", func(t *testing.T) {
		if got := FindWarpRouterDB(t.TempDir()); got != "" {
			t.Errorf("FindWarpRouterDB() = %q, want empty", got)
		}
	})
}

const warpTestSchema = `CREATE TABLE proxyPools (
	id TEXT PRIMARY KEY, isActive INTEGER, testStatus TEXT, data TEXT, createdAt TEXT, updatedAt TEXT)`

func warpSeedTestDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(warpTestSchema); err != nil {
		t.Fatal(err)
	}
	// A stale WARP row (must be replaced) and an unrelated row (must survive).
	if _, err := db.Exec(`INSERT INTO proxyPools (id, isActive, testStatus, data, createdAt, updatedAt)
		VALUES ('stale', 1, 'working', '{"name":"Cloudflare WARP Ultra-Fast (old)","proxyUrl":"http://127.0.0.1:9999"}', 'x', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO proxyPools (id, isActive, testStatus, data, createdAt, updatedAt)
		VALUES ('other', 1, 'working', '{"name":"some other pool"}', 'x', 'x')`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWarpSyncTo9Router(t *testing.T) {
	ctx := context.Background()
	t.Run("replaces stale row and preserves others", func(t *testing.T) {
		path := warpSeedTestDB(t)
		if err := SyncWarpTo9Router(ctx, 10808, path); err != nil {
			t.Fatalf("SyncWarpTo9Router() error = %v", err)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM proxyPools WHERE data LIKE '%Cloudflare WARP%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("expected exactly 1 WARP row, got %d", n)
		}
		var data string
		if err := db.QueryRow(`SELECT data FROM proxyPools WHERE data LIKE '%Cloudflare WARP%'`).Scan(&data); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("row data is not JSON: %v", err)
		}
		if m["proxyUrl"] != "http://127.0.0.1:10808" {
			t.Errorf("proxyUrl = %v", m["proxyUrl"])
		}
		if m["egressIp"] != "Cloudflare WARP Edge" {
			t.Errorf("egressIp = %v", m["egressIp"])
		}
		if m["strictProxy"] != false {
			t.Errorf("strictProxy = %v", m["strictProxy"])
		}
		if !strings.Contains(m["name"].(string), "Cloudflare WARP Ultra-Fast (http://127.0.0.1:10808)") {
			t.Errorf("name = %v", m["name"])
		}
		// Unrelated row untouched.
		if err := db.QueryRow(`SELECT COUNT(*) FROM proxyPools WHERE id = 'other'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("unrelated row must survive, n=%d err=%v", n, err)
		}

		// Idempotent: a second sync still leaves exactly one WARP row.
		if err := SyncWarpTo9Router(ctx, 10808, path); err != nil {
			t.Fatalf("second SyncWarpTo9Router() error = %v", err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM proxyPools WHERE data LIKE '%Cloudflare WARP%'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("expected 1 WARP row after second sync, n=%d err=%v", n, err)
		}
	})
	t.Run("no database is an error", func(t *testing.T) {
		t.Setenv("ROUTER_DB_PATH", "")
		t.Setenv("BANSOS_ROUTER_DB", "")
		t.Setenv("NINEROUTER_DB", "")
		if err := SyncWarpTo9Router(ctx, 10808, ""); err == nil {
			t.Fatal("expected error when no database is discoverable")
		}
		if err := SyncWarpTo9Router(ctx, 10808, filepath.Join(t.TempDir(), "missing.sqlite")); err == nil {
			t.Fatal("expected error for nonexistent db path")
		}
	})
}

func TestWarpGenerateAndSave(t *testing.T) {
	ctx := context.Background()
	srv := warpTestServer(t, `{"id":"dev-1","token":"tok","config":{"peers":[{"public_key":"PEERK","endpoint":{"host":"203.0.113.9:2408"}}],"interface":{"addresses":{"v4":"172.16.9.9/32","v6":""}}}}`, http.StatusOK)

	t.Run("writes conf and singbox files", func(t *testing.T) {
		out := t.TempDir()
		got, err := GenerateAndSaveWarp(ctx, WarpGenerateOptions{
			OutputDir: out, LocalPort: 10808, SyncDB: false,
			APIURL: srv.URL, Client: srv.Client(),
		})
		if err != nil {
			t.Fatalf("GenerateAndSaveWarp() error = %v", err)
		}
		if got.AccountID != "dev-1" || got.Endpoint != "203.0.113.9:2408" {
			t.Errorf("unexpected profile: %+v", got)
		}
		confBytes, err := os.ReadFile(filepath.Join(out, "warp.conf"))
		if err != nil {
			t.Fatalf("warp.conf missing: %v", err)
		}
		conf := string(confBytes)
		for _, want := range []string{"[Interface]", "PrivateKey = " + got.PrivateKey,
			"Address = 172.16.9.9/32", "[Peer]", "PublicKey = PEERK", "Endpoint = 203.0.113.9:2408"} {
			if !strings.Contains(conf, want) {
				t.Errorf("warp.conf missing %q:\n%s", want, conf)
			}
		}
		sbBytes, err := os.ReadFile(filepath.Join(out, "warp_singbox.json"))
		if err != nil {
			t.Fatalf("warp_singbox.json missing: %v", err)
		}
		var sb WarpSingboxConfig
		if err := json.Unmarshal(sbBytes, &sb); err != nil {
			t.Fatalf("sing-box file is not valid JSON: %v", err)
		}
		if sb.Inbounds[0].ListenPort != 10808 || sb.Outbounds[0].ServerPort != 2408 {
			t.Errorf("unexpected sing-box ports: %+v", sb)
		}
	})

	t.Run("sync db writes proxy pool row", func(t *testing.T) {
		out := t.TempDir()
		dbPath := warpSeedTestDB(t)
		if _, err := GenerateAndSaveWarp(ctx, WarpGenerateOptions{
			OutputDir: out, LocalPort: 10809, SyncDB: true, DBPath: dbPath,
			APIURL: srv.URL, Client: srv.Client(),
		}); err != nil {
			t.Fatalf("GenerateAndSaveWarp() error = %v", err)
		}
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM proxyPools WHERE data LIKE '%http://127.0.0.1:10809%'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("expected WARP row for :10809, n=%d err=%v", n, err)
		}
	})

	t.Run("registration failure propagates", func(t *testing.T) {
		bad := warpTestServer(t, `boom`, http.StatusInternalServerError)
		if _, err := GenerateAndSaveWarp(ctx, WarpGenerateOptions{
			OutputDir: t.TempDir(), APIURL: bad.URL, Client: bad.Client(),
		}); err == nil {
			t.Fatal("expected error when registration fails")
		}
	})
}
