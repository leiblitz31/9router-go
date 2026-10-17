package petaniproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFetcherParseProxiesFromText(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		protocol string
		expected []FetcherProxyCandidate
	}{
		{
			name:     "plain ip:port pair",
			content:  "1.2.3.4:8080",
			protocol: "http",
			expected: []FetcherProxyCandidate{{IP: "1.2.3.4", Port: 8080, Proxy: "1.2.3.4:8080", Protocol: "http"}},
		},
		{
			name:     "scheme prefix is ignored, caller protocol wins",
			content:  "socks5://5.6.7.8:1080",
			protocol: "http",
			expected: []FetcherProxyCandidate{{IP: "5.6.7.8", Port: 1080, Proxy: "5.6.7.8:1080", Protocol: "http"}},
		},
		{
			name:     "scheme match is case-insensitive",
			content:  "HTTP://9.9.9.9:3128",
			protocol: "http",
			expected: []FetcherProxyCandidate{{IP: "9.9.9.9", Port: 3128, Proxy: "9.9.9.9:3128", Protocol: "http"}},
		},
		{
			name:     "multiple proxies in noise",
			content:  "junk 10.0.0.1:80 more junk 10.0.0.2:443 trailing",
			protocol: "socks4",
			expected: []FetcherProxyCandidate{
				{IP: "10.0.0.1", Port: 80, Proxy: "10.0.0.1:80", Protocol: "socks4"},
				{IP: "10.0.0.2", Port: 443, Proxy: "10.0.0.2:443", Protocol: "socks4"},
			},
		},
		{
			name:     "port boundaries 80 and 65535 accepted",
			content:  "1.1.1.1:80 2.2.2.2:65535",
			protocol: "http",
			expected: []FetcherProxyCandidate{
				{IP: "1.1.1.1", Port: 80, Proxy: "1.1.1.1:80", Protocol: "http"},
				{IP: "2.2.2.2", Port: 65535, Proxy: "2.2.2.2:65535", Protocol: "http"},
			},
		},
		{
			name:     "port 0 and 65536 rejected",
			content:  "1.1.1.1:0 2.2.2.2:65536",
			protocol: "http",
			expected: nil,
		},
		{
			name:     "port 99999 (5 digits, out of range) rejected",
			content:  "1.1.1.1:99999",
			protocol: "http",
			expected: nil,
		},
		{
			name:     "invalid octet rejected",
			content:  "999.999.999.999:8080",
			protocol: "http",
			expected: nil,
		},
		{
			name:     "single digit port rejected",
			content:  "1.1.1.1:8",
			protocol: "http",
			expected: nil,
		},
		{
			name:     "empty content",
			content:  "",
			protocol: "http",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseProxiesFromText(tt.content, tt.protocol)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("parseProxiesFromText(%q, %q) = %+v, want %+v", tt.content, tt.protocol, got, tt.expected)
			}
		})
	}
}

func TestFetcherLoadSourcesConfig(t *testing.T) {
	dir := t.TempDir()

	validPath := filepath.Join(dir, "sources.json")
	validJSON := `{"http": ["https://a.example/x"], "socks5": ["https://b.example/y"]}`
	if err := os.WriteFile(validPath, []byte(validJSON), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		want    SourcesConfig
		wantErr bool
	}{
		{
			name: "valid file parses",
			path: validPath,
			want: SourcesConfig{"http": {"https://a.example/x"}, "socks5": {"https://b.example/y"}},
		},
		{
			name: "missing file yields default empty config",
			path: filepath.Join(dir, "nope.json"),
			want: defaultSourcesConfig(),
		},
		{
			name: "empty path yields default empty config",
			path: "",
			want: defaultSourcesConfig(),
		},
		{
			name:    "malformed json errors",
			path:    badPath,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadSourcesConfig(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("LoadSourcesConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LoadSourcesConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFetcherFetchSingleSource(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent = %q, want %q", got, userAgent)
		}
		fmt.Fprint(w, "1.2.3.4:8080\nnot a proxy\n5.6.7.8:1080")
	}))
	defer okSrv.Close()

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()

	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close() // refuse connections

	ctx := context.Background()

	tests := []struct {
		name     string
		url      string
		protocol string
		wantLen  int
	}{
		{name: "200 parses proxies", url: okSrv.URL, protocol: "http", wantLen: 2},
		{name: "non-200 yields empty", url: errSrv.URL, protocol: "http", wantLen: 0},
		{name: "dead host yields empty", url: deadURL, protocol: "socks5", wantLen: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FetchSingleSource(ctx, nil, tt.url, tt.protocol)
			if err != nil {
				t.Fatalf("FetchSingleSource() error = %v", err)
			}
			if len(got) != tt.wantLen {
				t.Errorf("FetchSingleSource() got %d proxies, want %d (%+v)", len(got), tt.wantLen, got)
			}
			for _, p := range got {
				if p.Protocol != tt.protocol {
					t.Errorf("candidate protocol = %q, want %q", p.Protocol, tt.protocol)
				}
			}
		})
	}
}

func TestFetcherApplyCountryFilter(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		country string
		want    string
	}{
		{
			name:    "rewrites country=all on proxyscrape",
			url:     "https://api.proxyscrape.com/v2/?request=getproxies&protocol=http&country=all",
			country: "id",
			want:    "https://api.proxyscrape.com/v2/?request=getproxies&protocol=http&country=ID",
		},
		{
			name:    "no filter leaves url untouched",
			url:     "https://api.proxyscrape.com/v2/?country=all",
			country: "",
			want:    "https://api.proxyscrape.com/v2/?country=all",
		},
		{
			name:    "non-proxyscrape url untouched",
			url:     "https://example.com/list?country=all",
			country: "id",
			want:    "https://example.com/list?country=all",
		},
		{
			name:    "proxyscrape without country=all untouched",
			url:     "https://api.proxyscrape.com/v2/?country=US",
			country: "id",
			want:    "https://api.proxyscrape.com/v2/?country=US",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := applyCountryFilter(tt.url, tt.country); got != tt.want {
				t.Errorf("applyCountryFilter(%q, %q) = %q, want %q", tt.url, tt.country, got, tt.want)
			}
		})
	}
}

func TestFetcherResolveProtocols(t *testing.T) {
	sources := SourcesConfig{
		"http":   {"u1"},
		"socks4": {"u2"},
		"socks5": {"u3"},
	}

	tests := []struct {
		name      string
		requested []string
		want      []string
	}{
		{name: "nil selects all sorted", requested: nil, want: []string{"http", "socks4", "socks5"}},
		{name: "empty selects all sorted", requested: []string{}, want: []string{"http", "socks4", "socks5"}},
		{name: "all keyword selects all", requested: []string{"all"}, want: []string{"http", "socks4", "socks5"}},
		{name: "mixed all keyword selects all", requested: []string{"http", "all"}, want: []string{"http", "socks4", "socks5"}},
		{name: "case-insensitive filter keeps request order", requested: []string{"SOCKS5", "http"}, want: []string{"socks5", "http"}},
		{name: "unknown protocol dropped", requested: []string{"http", "socks6"}, want: []string{"http"}},
		{
			name:      "uppercase ALL is not the keyword (python parity: case-sensitive)",
			requested: []string{"ALL"},
			want:      []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveProtocols(tt.requested, sources)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("resolveProtocols(%v) = %v, want %v", tt.requested, got, tt.want)
			}
		})
	}
}

// writeSourcesConfig writes a sources.json pointing at the given servers.
func writeSourcesConfig(t *testing.T, dir string, entries map[string][]string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("{")
	i := 0
	for proto, urls := range entries {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%q:[", proto)
		for j, u := range urls {
			if j > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "%q", u)
		}
		sb.WriteString("]")
		i++
	}
	sb.WriteString("}")
	path := filepath.Join(dir, "sources.json")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return path
}

func TestFetcherFetchProxies(t *testing.T) {
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "1.1.1.1:8080\n2.2.2.2:8080")
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "1.1.1.1:8080\n3.3.3.3:1080")
	}))
	defer srvB.Close()

	dir := t.TempDir()
	cfgPath := writeSourcesConfig(t, dir, map[string][]string{
		"http":   {srvA.URL},
		"socks5": {srvB.URL},
	})

	t.Run("dedupes across feeds, keeps protocols distinct", func(t *testing.T) {
		got, err := FetchProxies(context.Background(), FetchOptions{ConfigPath: cfgPath})
		if err != nil {
			t.Fatalf("FetchProxies() error = %v", err)
		}
		// 1.1.1.1:8080 appears in both feeds but under different protocols:
		// dedup key is protocol://ip:port, so it survives twice.
		if len(got) != 4 {
			t.Fatalf("FetchProxies() = %d candidates, want 4: %+v", len(got), got)
		}
		var httpCount, socks5Count int
		for _, p := range got {
			switch p.Protocol {
			case "http":
				httpCount++
			case "socks5":
				socks5Count++
			default:
				t.Errorf("unexpected protocol %q", p.Protocol)
			}
			if p.Proxy == "" || p.Port == 0 {
				t.Errorf("candidate missing fields: %+v", p)
			}
		}
		if httpCount != 2 || socks5Count != 2 {
			t.Errorf("protocol split = http:%d socks5:%d, want 2/2", httpCount, socks5Count)
		}
	})

	t.Run("protocol filter limits scraping", func(t *testing.T) {
		got, err := FetchProxies(context.Background(), FetchOptions{ConfigPath: cfgPath, Protocols: []string{"http"}})
		if err != nil {
			t.Fatalf("FetchProxies() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("FetchProxies() = %d candidates, want 2", len(got))
		}
		for _, p := range got {
			if p.Protocol != "http" {
				t.Errorf("candidate protocol = %q, want http", p.Protocol)
			}
		}
	})

	t.Run("unknown protocol yields empty", func(t *testing.T) {
		got, err := FetchProxies(context.Background(), FetchOptions{ConfigPath: cfgPath, Protocols: []string{"socks6"}})
		if err != nil {
			t.Fatalf("FetchProxies() error = %v", err)
		}
		if len(got) != 0 {
			t.Errorf("FetchProxies() = %d candidates, want 0", len(got))
		}
	})

	t.Run("missing config yields empty, no error", func(t *testing.T) {
		got, err := FetchProxies(context.Background(), FetchOptions{ConfigPath: filepath.Join(dir, "absent.json")})
		if err != nil {
			t.Fatalf("FetchProxies() error = %v", err)
		}
		if len(got) != 0 {
			t.Errorf("FetchProxies() = %d candidates, want 0", len(got))
		}
	})
}

func TestFetcherSameFeedDuplicates(t *testing.T) {
	// One feed listing the same proxy twice must yield it once.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "7.7.7.7:3128\n7.7.7.7:3128\n7.7.7.7:3128")
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := writeSourcesConfig(t, dir, map[string][]string{"http": {srv.URL}})

	got, err := FetchProxies(context.Background(), FetchOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("FetchProxies() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchProxies() = %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Proxy != "7.7.7.7:3128" || got[0].IP != "7.7.7.7" || got[0].Port != 3128 {
		t.Errorf("unexpected candidate: %+v", got[0])
	}
}
