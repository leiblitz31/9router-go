package petaniproxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestValidatorParseProxyLine(t *testing.T) {
	tests := []struct {
		name         string
		line         string
		defaultProto string
		wantIP       string
		wantPort     int
		wantProto    string
		wantOK       bool
	}{
		{"bare ip:port", "1.2.3.4:8080", "http", "1.2.3.4", 8080, "http", true},
		{"explicit socks5", "socks5://1.2.3.4:1080", "http", "1.2.3.4", 1080, "socks5", true},
		{"explicit http", "http://5.6.7.8:3128", "socks5", "5.6.7.8", 3128, "http", true},
		{"explicit https", "https://5.6.7.8:443", "http", "5.6.7.8", 443, "https", true},
		{"explicit socks4", "socks4://9.9.9.9:1080", "http", "9.9.9.9", 1080, "socks4", true},
		{"proto case-insensitive", "SOCKS5://1.2.3.4:1080", "http", "1.2.3.4", 1080, "socks5", true},
		{"embedded in text", "proxy 10.0.0.1:9050 is here", "socks5", "10.0.0.1", 9050, "socks5", true},
		{"min port digits", "1.2.3.4:80", "http", "1.2.3.4", 80, "http", true},
		{"max port", "1.2.3.4:65535", "http", "1.2.3.4", 65535, "http", true},
		{"port too big skipped", "1.2.3.4:99999", "http", "", 0, "", false},
		{"port zero skipped", "1.2.3.4:0", "http", "", 0, "", false},
		{"bad octet skipped", "999.999.999.999:8080", "http", "", 0, "", false},
		{"hostname skipped", "proxy.example.com:8080", "http", "", 0, "", false},
		{"no port skipped", "just some text", "http", "", 0, "", false},
		{"empty line", "", "http", "", 0, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseProxyLine(tt.line, tt.defaultProto)
			if ok != tt.wantOK {
				t.Fatalf("ParseProxyLine(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.IP != tt.wantIP || got.Port != tt.wantPort || got.Protocol != tt.wantProto {
				t.Errorf("ParseProxyLine(%q) = %+v, want %s:%d/%s", tt.line, got, tt.wantIP, tt.wantPort, tt.wantProto)
			}
			if want := tt.wantIP + ":" + fmt.Sprint(tt.wantPort); got.Proxy != want {
				t.Errorf("Proxy = %q, want %q", got.Proxy, want)
			}
		})
	}
}

func TestValidatorExtractProxies(t *testing.T) {
	text := "1.1.1.1:8080\nsocks5://2.2.2.2:1080 garbage\nnot-a-proxy\n3.3.3.3:99999\n"
	got := ExtractProxies(text, "http")
	if len(got) != 2 {
		t.Fatalf("ExtractProxies() found %d, want 2", len(got))
	}
	if got[0].Protocol != "http" || got[0].IP != "1.1.1.1" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Protocol != "socks5" || got[1].IP != "2.2.2.2" {
		t.Errorf("second = %+v", got[1])
	}
}

func TestValidatorFetchFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "1.1.1.1:8080\nsocks5://2.2.2.2:1080\njunk\n")
	}))
	defer srv.Close()

	got := FetchFeed(context.Background(), &http.Client{Timeout: 5 * time.Second}, srv.URL, "http")
	if len(got) != 2 {
		t.Fatalf("FetchFeed() = %d proxies, want 2", len(got))
	}

	t.Run("non-200 yields nothing", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusNotFound)
		}))
		defer bad.Close()
		if got := FetchFeed(context.Background(), &http.Client{Timeout: 5 * time.Second}, bad.URL, "http"); len(got) != 0 {
			t.Errorf("FetchFeed() on 404 = %d, want 0", len(got))
		}
	})

	t.Run("unreachable yields nothing", func(t *testing.T) {
		if got := FetchFeed(context.Background(), &http.Client{Timeout: 2 * time.Second}, "http://127.0.0.1:1/", "http"); len(got) != 0 {
			t.Errorf("FetchFeed() on dead host = %d, want 0", len(got))
		}
	})
}

func TestValidatorHarvestRawCandidates(t *testing.T) {
	feed1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "1.1.1.1:8080\n2.2.2.2:8080\n")
	}))
	defer feed1.Close()
	feed2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "2.2.2.2:8080\n3.3.3.3:1080\n")
	}))
	defer feed2.Close()

	sources := map[string][]string{"http": {feed1.URL, feed2.URL}}
	got := HarvestRawCandidates(context.Background(), []string{"http"}, sources)
	if len(got) != 3 {
		t.Fatalf("HarvestRawCandidates() = %d, want 3 deduped", len(got))
	}
	seen := map[string]bool{}
	for _, c := range got {
		key := c.Protocol + "://" + c.Proxy
		if seen[key] {
			t.Errorf("duplicate %s", key)
		}
		seen[key] = true
		if c.Protocol != "http" {
			t.Errorf("protocol = %q, want http", c.Protocol)
		}
	}
}

func TestValidatorCheckHTTPProxy(t *testing.T) {
	proxyAddr := startForwardProxy(t)
	target := startIPServer(t, "5.6.7.8", nil)

	t.Run("live proxy passes", func(t *testing.T) {
		v := NewValidator(ValidatorOptions{MaxLatency: 2 * time.Second, TestTarget: target})
		lp, err := v.CheckHTTPProxy(context.Background(), testCandidate(proxyAddr, "http"))
		if err != nil {
			t.Fatalf("CheckHTTPProxy() error = %v", err)
		}
		if lp.Protocol != "http" || lp.Proxy != proxyAddr {
			t.Errorf("unexpected result: %+v", lp)
		}
		if lp.URL != "http://"+proxyAddr {
			t.Errorf("URL = %q", lp.URL)
		}
		if lp.TestedAt.IsZero() {
			t.Error("TestedAt not set")
		}
	})

	t.Run("over-latency proxy rejected", func(t *testing.T) {
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			fmt.Fprint(w, `{"ip":"5.6.7.8"}`)
		}))
		defer slow.Close()
		v := NewValidator(ValidatorOptions{MaxLatency: 50 * time.Millisecond, TestTarget: slow.URL})
		if _, err := v.CheckHTTPProxy(context.Background(), testCandidate(proxyAddr, "http")); err == nil {
			t.Error("CheckHTTPProxy() expected too-slow error, got nil")
		}
	})

	t.Run("dead proxy rejected", func(t *testing.T) {
		v := NewValidator(ValidatorOptions{MaxLatency: time.Second, TestTarget: target})
		if _, err := v.CheckHTTPProxy(context.Background(), testCandidate("127.0.0.1:1", "http")); err == nil {
			t.Error("CheckHTTPProxy() expected error, got nil")
		}
	})

	t.Run("defaults applied", func(t *testing.T) {
		v := NewValidator(ValidatorOptions{})
		if v.maxLatency != 350*time.Millisecond || v.testTarget != DefaultTestTarget {
			t.Errorf("defaults not applied: %+v", v)
		}
	})
}

func TestValidatorCheckSocksProxy(t *testing.T) {
	target := startIPServer(t, "5.6.7.8", nil)
	v := NewValidator(ValidatorOptions{MaxLatency: 5 * time.Second, TestTarget: target})

	t.Run("socks5", func(t *testing.T) {
		addr := startSocks5Server(t)
		lp, err := v.CheckSocksProxy(context.Background(), testCandidate(addr, "socks5"))
		if err != nil {
			t.Fatalf("CheckSocksProxy() socks5 error = %v", err)
		}
		if lp.Protocol != "socks5" || lp.URL != "socks5://"+addr {
			t.Errorf("unexpected result: %+v", lp)
		}
	})

	t.Run("socks4", func(t *testing.T) {
		addr := startSocks4Server(t)
		lp, err := v.CheckSocksProxy(context.Background(), testCandidate(addr, "socks4"))
		if err != nil {
			t.Fatalf("CheckSocksProxy() socks4 error = %v", err)
		}
		if lp.Protocol != "socks4" {
			t.Errorf("unexpected result: %+v", lp)
		}
	})

	t.Run("empty protocol defaults to socks5", func(t *testing.T) {
		addr := startSocks5Server(t)
		c := testCandidate(addr, "")
		lp, err := v.CheckSocksProxy(context.Background(), c)
		if err != nil {
			t.Fatalf("CheckSocksProxy() error = %v", err)
		}
		if lp.Protocol != "socks5" {
			t.Errorf("Protocol = %q, want socks5", lp.Protocol)
		}
	})
}

func TestValidatorValidateBatch(t *testing.T) {
	proxyAddr := startForwardProxy(t)
	target := startIPServer(t, "5.6.7.8", nil)
	v := NewValidator(ValidatorOptions{MaxLatency: 5 * time.Second, TestTarget: target})

	candidates := []ProxyCandidate{
		testCandidate(proxyAddr, "http"),
		testCandidate(proxyAddr, "http"),
		testCandidate("127.0.0.1:1", "http"), // dead
	}

	t.Run("early stop at target", func(t *testing.T) {
		var calls int32
		got := v.ValidateBatch(context.Background(), candidates, 4, 1, func(lp *LiveProxy) {
			atomic.AddInt32(&calls, 1)
		})
		if len(got) != 1 {
			t.Fatalf("ValidateBatch() = %d, want 1", len(got))
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Errorf("onLive called %d times, want 1", n)
		}
	})

	t.Run("no early stop collects all live", func(t *testing.T) {
		got := v.ValidateBatch(context.Background(), candidates, 4, 0, nil)
		if len(got) != 2 {
			t.Fatalf("ValidateBatch() = %d, want 2", len(got))
		}
	})

	t.Run("mixed protocols dispatch", func(t *testing.T) {
		socksAddr := startSocks5Server(t)
		mixed := []ProxyCandidate{
			testCandidate(proxyAddr, "http"),
			testCandidate(socksAddr, "socks5"),
		}
		got := v.ValidateBatch(context.Background(), mixed, 4, 0, nil)
		if len(got) != 2 {
			t.Fatalf("ValidateBatch() = %d, want 2", len(got))
		}
		protos := map[string]bool{}
		for _, lp := range got {
			protos[lp.Protocol] = true
		}
		if !protos["http"] || !protos["socks5"] {
			t.Errorf("protocols = %v, want http+socks5", protos)
		}
	})
}

func createProxyPoolsTable(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE proxyPools (
		id TEXT PRIMARY KEY, isActive INTEGER, testStatus TEXT,
		data TEXT, createdAt TEXT, updatedAt TEXT)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
}

func TestValidatorSyncToSQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	createProxyPoolsTable(t, dbPath)

	now := time.Now().UTC()
	proxies := []*LiveProxy{
		{IP: "1.1.1.1", Port: 8080, Proxy: "1.1.1.1:8080", Protocol: "http", URL: "http://1.1.1.1:8080", LatencyMs: 120, TestedAt: now},
		{IP: "2.2.2.2", Port: 1080, Proxy: "2.2.2.2:1080", Protocol: "socks5", URL: "socks5://2.2.2.2:1080", LatencyMs: 230, TestedAt: now},
	}
	n, err := SyncToSQLite(proxies, dbPath)
	if err != nil {
		t.Fatalf("SyncToSQLite() error = %v", err)
	}
	if n != 2 {
		t.Fatalf("SyncToSQLite() = %d, want 2", n)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM proxyPools WHERE testStatus='working'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("rows = %d, want 2", count)
	}
	var data string
	if err := db.QueryRow(`SELECT data FROM proxyPools WHERE data LIKE '%1.1.1.1%'`).Scan(&data); err != nil {
		t.Fatalf("select data: %v", err)
	}
	var payload struct {
		Name     string  `json:"name"`
		ProxyURL string  `json:"proxyUrl"`
		Type     string  `json:"type"`
		Latency  float64 `json:"latency"`
		EgressIP string  `json:"egressIp"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.ProxyURL != "http://1.1.1.1:8080" || payload.Type != "http" || payload.EgressIP != "1.1.1.1" {
		t.Errorf("payload = %+v", payload)
	}
	if payload.Latency != 0.12 {
		t.Errorf("latency = %v, want 0.12", payload.Latency)
	}
	if !strings.Contains(payload.Name, "HTTP") || !strings.Contains(payload.Name, "120ms") {
		t.Errorf("name = %q", payload.Name)
	}

	t.Run("missing db returns zero", func(t *testing.T) {
		n, err := SyncToSQLite(proxies, filepath.Join(t.TempDir(), "nope.sqlite"))
		if err != nil || n != 0 {
			t.Errorf("SyncToSQLite() = (%d, %v), want (0, nil)", n, err)
		}
	})
	t.Run("empty path returns zero", func(t *testing.T) {
		n, err := SyncToSQLite(proxies, "")
		if err != nil || n != 0 {
			t.Errorf("SyncToSQLite() = (%d, %v), want (0, nil)", n, err)
		}
	})
}

func TestValidatorFind9RouterDB(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "data.sqlite")
	if err := os.WriteFile(dbFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_DB_PATH", dbFile)
	t.Setenv("BANSOS_ROUTER_DB", "")
	t.Setenv("NINEROUTER_DB", "")
	if got := Find9RouterDB(); got != dbFile {
		t.Errorf("Find9RouterDB() = %q, want %q", got, dbFile)
	}

	t.Run("nonexistent env path is skipped", func(t *testing.T) {
		t.Setenv("ROUTER_DB_PATH", filepath.Join(t.TempDir(), "missing.sqlite"))
		if got := Find9RouterDB(); got == os.Getenv("ROUTER_DB_PATH") {
			t.Errorf("Find9RouterDB() returned nonexistent path %q", got)
		}
	})
}

func TestValidatorRunFastHarvester(t *testing.T) {
	proxyAddr := startForwardProxy(t)
	target := startIPServer(t, "5.6.7.8", nil)

	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One live proxy (listed twice -> deduped), one dead.
		fmt.Fprintf(w, "%s\n%s\n127.0.0.1:1\n", proxyAddr, proxyAddr)
	}))
	defer feed.Close()

	dbPath := filepath.Join(t.TempDir(), "harvest.sqlite")
	createProxyPoolsTable(t, dbPath)
	outFile := filepath.Join(t.TempDir(), "out", "fast_elite.txt")

	var liveCalls int32
	opts := HarvestOptions{
		Protocols:    []string{"http"},
		Sources:      map[string][]string{"http": {feed.URL}},
		MaxLatencyMs: 3000,
		TargetCount:  5,
		MaxWorkers:   8,
		BatchSize:    10,
		TestTarget:   target,
		SyncDB:       true,
		DBPath:       dbPath,
		OutputFile:   outFile,
		OnLive:       func(lp *LiveProxy) { atomic.AddInt32(&liveCalls, 1) },
	}
	live, err := RunFastHarvester(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunFastHarvester() error = %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("RunFastHarvester() = %d live, want 1", len(live))
	}
	if n := atomic.LoadInt32(&liveCalls); n != 1 {
		t.Errorf("OnLive called %d times, want 1", n)
	}

	content, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if want := "http://" + proxyAddr + "\n"; string(content) != want {
		t.Errorf("output file = %q, want %q", content, want)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM proxyPools`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("db rows = %d, want 1", count)
	}

	t.Run("no candidates returns nil", func(t *testing.T) {
		empty := HarvestOptions{
			Protocols: []string{"http"},
			Sources:   map[string][]string{"http": {"http://127.0.0.1:1/feed"}},
		}
		got, err := RunFastHarvester(context.Background(), empty)
		if err != nil || len(got) != 0 {
			t.Errorf("RunFastHarvester() = (%v, %v), want (nil, nil)", got, err)
		}
	})
}
