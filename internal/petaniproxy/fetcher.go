// Package petaniproxy is a Go port of the petani-proxy OmniProxy Harvester
// fetcher (core/fetcher.py): a concurrent multi-feed proxy harvester that
// downloads and parses free public proxy feeds across HTTP, SOCKS4 and SOCKS5.
//
// Behavioral parity notes (Python -> Go):
//   - The Python dual engine (httpx async with urllib fallback) collapses into
//     a single net/http engine: Go has no missing-dependency fallback case.
//   - Python verify=False maps to InsecureSkipVerify on the default client.
//   - Per-source failures are swallowed and yield empty batches, like the
//     Python `except Exception: pass` in fetch_single_source_*.
//   - Python builds target protocols from dict insertion (JSON) order; Go
//     sorts protocol keys so output ordering is deterministic.
//   - The "all" protocol shortcut is case-sensitive, mirroring Python's
//     `"all" in protocols` membership test.
package petaniproxy

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// DefaultMaxWorkers limits concurrent fetches. Reduced from Python's 30
// because the egress proxy rate-limits aggressive concurrency (30 parallel
// = 0 results, 5 parallel = works).
const DefaultMaxWorkers = 5

// DefaultRequestTimeout mirrors the httpx timeout=12.0 of the primary engine.
const DefaultRequestTimeout = 12 * time.Second

// userAgent mirrors the UA header used by both Python engines.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64)"

// proxyRegex captures IPv4:Port pairs with optional http/https/socks4/socks5
// scheme prefixes. The captured scheme is ignored: the caller's protocol wins,
// exactly like _parse_proxies_from_text in fetcher.py.
var proxyRegex = regexp.MustCompile(
	`(?i)(?:(?P<proto>https?|socks4|socks5)://)?` +
		`(?P<ip>(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(?:\.(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3})` +
		`:(?P<port>\d{2,5})`,
)

// FetcherProxyCandidate is a single proxy harvested from a public feed.
// Other petaniproxy modules should treat this as the shared candidate type
// until the coordinator reconciles a common Proxy struct.
type FetcherProxyCandidate struct {
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	Proxy    string `json:"proxy"`    // "ip:port"
	Protocol string `json:"protocol"` // "http" | "socks4" | "socks5"
}

// SourcesConfig maps protocol -> list of feed URLs, as loaded from
// config/sources.json.
type SourcesConfig map[string][]string

// FetchTarget is one feed URL together with the protocol its proxies
// are tagged with.
type FetchTarget struct {
	URL      string
	Protocol string
}

// FetchOptions configures FetchProxies.
type FetchOptions struct {
	// Protocols selects which protocols to scrape; nil or containing "all"
	// scrapes every protocol present in the sources config.
	Protocols []string
	// CountryFilter rewrites "country=all" on proxyscrape.com URLs to the
	// given ISO country code (upper-cased).
	CountryFilter string
	// ConfigPath is the path to sources.json. Empty means "no sources":
	// returns an empty config (the Python __file__-relative default path
	// has no Go equivalent).
	ConfigPath string
	// Verbose prints harvest progress to stdout.
	Verbose bool
	// Client overrides DefaultClient when non-nil.
	Client *http.Client
}

// DefaultClient is a package-level shared client: 12s timeout, follows
// redirects, skips TLS verification (parity with httpx verify=False) and
// honors proxy env vars.
var DefaultClient = &http.Client{
	Timeout: DefaultRequestTimeout,
	Transport: &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // parity with verify=False
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: 10,
	},
}

//go:embed sources.json
var embeddedSourcesJSON []byte

// defaultSourcesConfig returns the embedded sources.json (ported from
// Python config/sources.json). Falls back to empty if embed fails.
func defaultSourcesConfig() SourcesConfig {
	var cfg SourcesConfig
	if err := json.Unmarshal(embeddedSourcesJSON, &cfg); err == nil && cfg != nil {
		return cfg
	}
	return SourcesConfig{"http": {}, "socks4": {}, "socks5": {}}
}

// LoadSourcesConfig loads the feed list from a JSON configuration file.
// A missing file (or empty path) yields the default empty config, like
// fetcher.py's os.path.exists branch; malformed JSON returns an error,
// like json.load raising.
func LoadSourcesConfig(configPath string) (SourcesConfig, error) {
	if configPath == "" {
		return defaultSourcesConfig(), nil
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultSourcesConfig(), nil
		}
		return nil, fmt.Errorf("petaniproxy.LoadSourcesConfig: %w", err)
	}
	var cfg SourcesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("petaniproxy.LoadSourcesConfig: %w", err)
	}
	if cfg == nil {
		return defaultSourcesConfig(), nil
	}
	return cfg, nil
}

// parseProxiesFromText extracts IPv4:Port candidates from free text and tags
// them with protocol, mirroring _parse_proxies_from_text (ports must be in
// 1..65535).
func parseProxiesFromText(content, protocol string) []FetcherProxyCandidate {
	names := proxyRegex.SubexpNames()
	var ipIdx, portIdx = -1, -1
	for i, n := range names {
		switch n {
		case "ip":
			ipIdx = i
		case "port":
			portIdx = i
		}
	}

	var proxies []FetcherProxyCandidate
	for _, m := range proxyRegex.FindAllStringSubmatch(content, -1) {
		port, err := strconv.Atoi(m[portIdx])
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		ip := m[ipIdx]
		proxies = append(proxies, FetcherProxyCandidate{
			IP:       ip,
			Port:     port,
			Proxy:    ip + ":" + m[portIdx],
			Protocol: protocol,
		})
	}
	return proxies
}

// FetchSingleSource downloads one feed URL and parses proxy candidates from
// it, mirroring fetch_single_source_async / fetch_single_source_urllib.
// Only HTTP 200 responses are parsed; any failure returns an empty slice
// (parity with the Python `except Exception: pass`).
func FetchSingleSource(ctx context.Context, client *http.Client, url, protocol string) ([]FetcherProxyCandidate, error) {
	if client == nil {
		client = DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy.FetchSingleSource: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil
	}
	return parseProxiesFromText(string(body), protocol), nil
}

// applyCountryFilter mirrors the Python proxyscrape country rewrite:
// replace "country=all" with "country=<FILTER>" (upper-cased) only for
// proxyscrape.com URLs, and only when a filter is set.
func applyCountryFilter(rawURL, country string) string {
	if country == "" {
		return rawURL
	}
	if !strings.Contains(rawURL, "proxyscrape.com") || !strings.Contains(rawURL, "country=all") {
		return rawURL
	}
	return strings.ReplaceAll(rawURL, "country=all", "country="+strings.ToUpper(country))
}

// FetchAllSources fetches every target concurrently (max DefaultMaxWorkers
// in flight) and returns per-target batches in target order, mirroring
// fetch_all_proxies_httpx (asyncio.gather preserves order) while capping
// concurrency like the threaded fallback. Per-source errors are ignored so
// one dead feed never fails the harvest.
func FetchAllSources(ctx context.Context, targets []FetchTarget, opts FetchOptions) [][]FetcherProxyCandidate {
	results := make([][]FetcherProxyCandidate, len(targets))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(DefaultMaxWorkers)
	for i, t := range targets {
		g.Go(func() error {
			batch, _ := FetchSingleSource(gctx, opts.Client, t.URL, t.Protocol)
			results[i] = batch
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// resolveProtocols mirrors the Python target_protocols selection: nil/empty
// or containing "all" selects every protocol in the sources config (sorted
// for determinism); otherwise each requested protocol is lower-cased and
// kept only if present in the config, preserving request order.
func resolveProtocols(requested []string, sources SourcesConfig) []string {
	protos := make([]string, 0, len(sources))
	if len(requested) == 0 {
		protos = append(protos, keysOf(sources)...)
		return protos
	}
	for _, p := range requested {
		if p == "all" {
			return keysOf(sources)
		}
	}
	for _, p := range requested {
		if lp := strings.ToLower(p); containsSource(sources, lp) {
			protos = append(protos, lp)
		}
	}
	return protos
}

func keysOf(sources SourcesConfig) []string {
	keys := make([]string, 0, len(sources))
	for k := range sources {
		keys = append(keys, k)
	}
	// insertion sort over a tiny slice: deterministic, no extra import.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func containsSource(sources SourcesConfig, proto string) bool {
	_, ok := sources[proto]
	return ok
}

// FetchProxies scrapes all endpoints across the requested protocols and
// returns deduplicated proxy candidates, mirroring fetch_proxies_sync /
// fetch_all_proxies. Dedup key is "protocol://ip:port".
func FetchProxies(ctx context.Context, opts FetchOptions) ([]FetcherProxyCandidate, error) {
	sources, err := LoadSourcesConfig(opts.ConfigPath)
	if err != nil {
		return nil, err
	}

	targetProtocols := resolveProtocols(opts.Protocols, sources)

	var targets []FetchTarget
	for _, proto := range targetProtocols {
		for _, url := range sources[proto] {
			targets = append(targets, FetchTarget{
				URL:      applyCountryFilter(url, opts.CountryFilter),
				Protocol: proto,
			})
		}
	}

	countryInfo := ""
	if opts.CountryFilter != "" {
		countryInfo = " (Country: " + strings.ToUpper(opts.CountryFilter) + ")"
	}
	if opts.Verbose {
		fmt.Printf("🌐 Fetching raw proxies from %d verified source feeds%s...\n", len(targets), countryInfo)
	}

	batchResults := FetchAllSources(ctx, targets, opts)

	seen := make(map[string]struct{})
	var unique []FetcherProxyCandidate
	for _, batch := range batchResults {
		for _, item := range batch {
			key := item.Protocol + "://" + item.Proxy
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			unique = append(unique, item)
		}
	}

	if opts.Verbose {
		fmt.Printf("📦 Total unique candidates harvested: %d proxies\n", len(unique))
		for _, proto := range targetProtocols {
			cnt := 0
			for _, p := range unique {
				if p.Protocol == proto {
					cnt++
				}
			}
			fmt.Printf("  • %s: %d candidates\n", strings.ToUpper(proto), cnt)
		}
	}
	return unique, nil
}
