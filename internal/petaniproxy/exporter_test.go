package petaniproxy

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func sampleExportProxies() []PoolProxy {
	mk := func(ip string, port int, proto, anon string, cc string, latencyMs float64) PoolProxy {
		return PoolProxy{
			FetcherProxyCandidate: FetcherProxyCandidate{
				IP:       ip,
				Port:     port,
				Proxy:    ip + ":" + strconv.Itoa(port),
				Protocol: proto,
			},
			Anonymity:   anon,
			LatencyMs:   latencyMs,
			LatencySec:  latencyMs / 1000,
			CountryCode: cc,
		}
	}
	return []PoolProxy{
		mk("1.1.1.1", 8080, "http", "Elite", "US", 120),
		mk("2.2.2.2", 1080, "socks5", "Anonymous", "DE", 340),
		mk("3.3.3.3", 1080, "socks4", "Transparent", "SG", 510),
		mk("4.4.4.4", 3128, "http", "Elite", "ID", 95),
		// Defaults: empty protocol -> http, empty anonymity -> Elite.
		{FetcherProxyCandidate: FetcherProxyCandidate{IP: "5.5.5.5", Port: 8080, Proxy: "5.5.5.5:8080"}},
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestExporterAllFormats(t *testing.T) {
	dir := t.TempDir()
	proxies := sampleExportProxies()

	got, err := ExportAllFormats(proxies, dir, nil)
	if err != nil {
		t.Fatalf("ExportAllFormats: %v", err)
	}

	wantKeys := []string{"all_txt", "urls_txt", "elite_txt", "http_txt", "socks4_txt", "socks5_txt", "json", "csv"}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("result map missing key %q", k)
		}
	}
	if _, ok := got["9router_db"]; ok {
		t.Error("result map has 9router_db key with nil db")
	}

	// live_all.txt: raw ip:port lines.
	if content := readTestFile(t, got["all_txt"]); content != "1.1.1.1:8080\n2.2.2.2:1080\n3.3.3.3:1080\n4.4.4.4:3128\n5.5.5.5:8080\n" {
		t.Errorf("live_all.txt =\n%q", content)
	}

	// live_urls.txt: proto://ip:port lines.
	urls := readTestFile(t, got["urls_txt"])
	for _, want := range []string{"http://1.1.1.1:8080", "socks5://2.2.2.2:1080", "socks4://3.3.3.3:1080", "http://5.5.5.5:8080"} {
		if !strings.Contains(urls, want+"\n") {
			t.Errorf("live_urls.txt missing %q", want)
		}
	}

	// live_elite.txt: only Elite (incl. default-anonymity) URLs.
	elite := readTestFile(t, got["elite_txt"])
	if elite != "http://1.1.1.1:8080\nhttp://4.4.4.4:3128\nhttp://5.5.5.5:8080\n" {
		t.Errorf("live_elite.txt =\n%q", elite)
	}

	// Per-protocol files: fixed set, correct buckets.
	protoCases := map[string]string{
		"http_txt":   "1.1.1.1:8080\n4.4.4.4:3128\n5.5.5.5:8080\n",
		"socks4_txt": "3.3.3.3:1080\n",
		"socks5_txt": "2.2.2.2:1080\n",
	}
	for key, want := range protoCases {
		if content := readTestFile(t, got[key]); content != want {
			t.Errorf("%s =\n%q, want\n%q", key, content, want)
		}
	}

	// proxies.json payload.
	var payload ProxyExportPayload
	if err := json.Unmarshal([]byte(readTestFile(t, got["json"])), &payload); err != nil {
		t.Fatalf("unmarshal proxies.json: %v", err)
	}
	if payload.TotalAlive != 5 {
		t.Errorf("total_alive = %d, want 5", payload.TotalAlive)
	}
	if payload.Protocols["http"] != 3 || payload.Protocols["socks4"] != 1 || payload.Protocols["socks5"] != 1 {
		t.Errorf("protocols = %v", payload.Protocols)
	}
	if payload.Anonymity["Elite"] != 3 || payload.Anonymity["Anonymous"] != 1 || payload.Anonymity["Transparent"] != 1 {
		t.Errorf("anonymity = %v", payload.Anonymity)
	}
	if len(payload.Proxies) != 5 {
		t.Errorf("proxies len = %d, want 5", len(payload.Proxies))
	}
	if payload.GeneratedAt.IsZero() {
		t.Error("generated_at not set")
	}

	// proxies.csv: header + 5 rows.
	records, err := csv.NewReader(strings.NewReader(readTestFile(t, got["csv"]))).ReadAll()
	if err != nil {
		t.Fatalf("parse proxies.csv: %v", err)
	}
	wantHeader := []string{"protocol", "ip", "port", "proxy", "anonymity", "latency_ms", "country_code", "country", "city", "isp", "egress_ip"}
	if len(records) != 6 {
		t.Fatalf("csv rows = %d, want 6 (header + 5)", len(records))
	}
	for i, h := range wantHeader {
		if records[0][i] != h {
			t.Errorf("csv header[%d] = %q, want %q", i, records[0][i], h)
		}
	}
	// Spot-check first row: http,Elite with latency 120.
	if records[1][0] != "http" || records[1][3] != "1.1.1.1:8080" || records[1][4] != "Elite" || records[1][5] != "120" || records[1][6] != "US" {
		t.Errorf("csv row 1 = %v", records[1])
	}
}

func TestExporterEmptyPool(t *testing.T) {
	dir := t.TempDir()
	got, err := ExportAllFormats(nil, dir, nil)
	if err != nil {
		t.Fatalf("ExportAllFormats: %v", err)
	}
	for _, key := range []string{"all_txt", "urls_txt", "elite_txt", "http_txt", "socks4_txt", "socks5_txt"} {
		if content := readTestFile(t, got[key]); content != "" {
			t.Errorf("%s: want empty file, got %q", key, content)
		}
	}
	var payload ProxyExportPayload
	if err := json.Unmarshal([]byte(readTestFile(t, got["json"])), &payload); err != nil {
		t.Fatalf("unmarshal proxies.json: %v", err)
	}
	if payload.TotalAlive != 0 || len(payload.Proxies) != 0 {
		t.Errorf("empty export: total_alive=%d proxies=%d", payload.TotalAlive, len(payload.Proxies))
	}
	records, err := csv.NewReader(strings.NewReader(readTestFile(t, got["csv"]))).ReadAll()
	if err != nil {
		t.Fatalf("parse proxies.csv: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("csv rows = %d, want 1 (header only)", len(records))
	}
}

func TestExporterDefaultDir(t *testing.T) {
	// Empty outputDir falls back to "output" (created by MkdirAll).
	// Run inside t.TempDir() via chdir to avoid polluting the repo.
	workdir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	got, err := ExportAllFormats(sampleExportProxies()[:1], "", nil)
	if err != nil {
		t.Fatalf("ExportAllFormats: %v", err)
	}
	if !strings.HasPrefix(got["all_txt"], filepath.Join("output", "live_all.txt")) {
		t.Errorf("all_txt = %q, want under output/", got["all_txt"])
	}
}

func TestExporterWriteProxyLinesTo(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteProxyLinesTo(&buf, []string{"a:1", "b:2"}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "a:1\nb:2\n" {
		t.Errorf("got %q", buf.String())
	}
	buf.Reset()
	if err := WriteProxyLinesTo(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("empty input wrote %q", buf.String())
	}
}

func openTestProxyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE proxyPools (
		id TEXT PRIMARY KEY, isActive INTEGER, testStatus TEXT,
		data TEXT, createdAt TEXT, updatedAt TEXT
	)`)
	if err != nil {
		t.Fatalf("create proxyPools: %v", err)
	}
	return db
}

func seedTestProxyRow(t *testing.T, db *sql.DB, id, status, data string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO proxyPools (id, isActive, testStatus, data, createdAt, updatedAt) VALUES (?, 1, ?, ?, ?, ?)`,
		id, status, data, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}
}

func countTestProxyRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM proxyPools`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func TestExporterSyncTo9RouterDB(t *testing.T) {
	newDB := func() *sql.DB {
		db := openTestProxyDB(t)
		seedTestProxyRow(t, db, "old-petani", "working", `{"name":"PetaniProxy [US] 9.9.9.9:1 (http)"}`)
		seedTestProxyRow(t, db, "old-unknown", "unknown", `{"name":"something else"}`)
		seedTestProxyRow(t, db, "keep-me", "working", `{"name":"Manual Pool"}`)
		return db
	}
	proxies := sampleExportProxies()[:2]

	t.Run("replace cleans and inserts", func(t *testing.T) {
		db := newDB()
		inserted, err := SyncTo9RouterDB(db, proxies, true)
		if err != nil {
			t.Fatalf("SyncTo9RouterDB: %v", err)
		}
		if inserted != 2 {
			t.Errorf("inserted = %d, want 2", inserted)
		}
		// old PetaniProxy row + unknown row gone, manual row kept, 2 new rows.
		if got := countTestProxyRows(t, db); got != 3 {
			t.Errorf("rows = %d, want 3", got)
		}
		var data, status string
		err = db.QueryRow(`SELECT data, testStatus FROM proxyPools WHERE data LIKE '%1.1.1.1:8080%'`).Scan(&data, &status)
		if err != nil {
			t.Fatalf("query inserted row: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if payload["name"] != "PetaniProxy [US] 1.1.1.1:8080 (http)" {
			t.Errorf("name = %v", payload["name"])
		}
		if payload["proxyUrl"] != "http://1.1.1.1:8080" {
			t.Errorf("proxyUrl = %v", payload["proxyUrl"])
		}
		if payload["latency"] != 0.12 {
			t.Errorf("latency = %v, want 0.12 (latency_sec)", payload["latency"])
		}
		if payload["egressIp"] != "1.1.1.1" { // falls back to IP when egress_ip empty
			t.Errorf("egressIp = %v", payload["egressIp"])
		}
		if payload["country"] != "US" {
			t.Errorf("country = %v", payload["country"])
		}
		if status != "working" {
			t.Errorf("testStatus = %q, want working", status)
		}
		// UUID ids are unique across the two inserts.
		var ids []string
		rows, err := db.Query(`SELECT id FROM proxyPools WHERE data LIKE '%PetaniProxy [%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if len(ids) != 2 || ids[0] == ids[1] {
			t.Errorf("ids not unique: %v", ids)
		}
		for _, id := range ids {
			if len(id) != 36 || id[14] != '4' {
				t.Errorf("id %q not a v4 UUID", id)
			}
		}
	})

	t.Run("no replace keeps existing", func(t *testing.T) {
		db := newDB()
		inserted, err := SyncTo9RouterDB(db, proxies, false)
		if err != nil {
			t.Fatalf("SyncTo9RouterDB: %v", err)
		}
		if inserted != 2 {
			t.Errorf("inserted = %d, want 2", inserted)
		}
		if got := countTestProxyRows(t, db); got != 5 {
			t.Errorf("rows = %d, want 5 (3 seeded + 2 new)", got)
		}
	})

	t.Run("nil db errors", func(t *testing.T) {
		if _, err := SyncTo9RouterDB(nil, proxies, true); err == nil {
			t.Error("want error for nil db")
		}
	})

	t.Run("latency defaults to 1.0", func(t *testing.T) {
		db := openTestProxyDB(t)
		p := PoolProxy{FetcherProxyCandidate: FetcherProxyCandidate{IP: "6.6.6.6", Port: 8080, Proxy: "6.6.6.6:8080"}}
		if _, err := SyncTo9RouterDB(db, []PoolProxy{p}, true); err != nil {
			t.Fatal(err)
		}
		var data string
		if err := db.QueryRow(`SELECT data FROM proxyPools`).Scan(&data); err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["latency"] != 1.0 {
			t.Errorf("latency = %v, want 1.0 default", payload["latency"])
		}
		if payload["country"] != "??" {
			t.Errorf("country = %v, want ?? default", payload["country"])
		}
	})
}

func TestExporterSyncViaExportAllFormats(t *testing.T) {
	dir := t.TempDir()
	db := openTestProxyDB(t)
	got, err := ExportAllFormats(sampleExportProxies()[:1], dir, db)
	if err != nil {
		t.Fatalf("ExportAllFormats: %v", err)
	}
	if got["9router_db"] != "synced:1" {
		t.Errorf("9router_db = %q, want synced:1", got["9router_db"])
	}
	if n := countTestProxyRows(t, db); n != 1 {
		t.Errorf("db rows = %d, want 1", n)
	}
}
