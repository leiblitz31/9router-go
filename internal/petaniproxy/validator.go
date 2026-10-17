package petaniproxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	_ "modernc.org/sqlite"
)

// DefaultTestTarget is the probe URL used by the fast validator, same as
// fast_validator.py's DEFAULT_TEST_TARGET.
const DefaultTestTarget = "http://api.ipify.org?format=json"

// FastSources mirrors fast_validator.py's FAST_SOURCES: public feeds per
// protocol. Callers may pass their own map to HarvestRawCandidates.
var FastSources = map[string][]string{
	"http": {
		"https://api.proxyscrape.com/v2/?request=displayproxies&protocol=http&timeout=3000&country=all&ssl=all&anonymity=elite",
		"https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/http.txt",
		"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/http.txt",
		"https://raw.githubusercontent.com/clarketm/proxy-list/master/proxy-list-raw.txt",
		"https://raw.githubusercontent.com/sunny9577/proxy-scraper/master/proxies.txt",
		"https://raw.githubusercontent.com/roosterkid/openproxylist/main/HTTPS_RAW.txt",
	},
	"socks5": {
		"https://api.proxyscrape.com/v2/?request=displayproxies&protocol=socks5&timeout=3000&country=all",
		"https://raw.githubusercontent.com/TheSpeedX/PROXY-List/master/socks5.txt",
		"https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt",
		"https://raw.githubusercontent.com/hookzof/socks5_list/master/proxy.txt",
	},
}

// proxyLinePattern is the Go equivalent of fast_validator.py's PROXY_REGEX:
// optional proto prefix, strict IPv4, port of 2-5 digits (range-checked to
// 1-65535 after matching).
var proxyLinePattern = regexp.MustCompile(
	`(?i)(?:(https?|socks4|socks5)://)?` +
		`((?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(?:\.(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3})` +
		`:(\d{2,5})`,
)

// ParseProxyLine extracts one proxy from a line of text. The protocol
// defaults to defaultProto when the line carries no "proto://" prefix.
// It reports false for lines without a valid ip:port.
func ParseProxyLine(line, defaultProto string) (ProxyCandidate, bool) {
	m := proxyLinePattern.FindStringSubmatch(line)
	if m == nil {
		return ProxyCandidate{}, false
	}
	port, err := strconv.Atoi(m[3])
	if err != nil || port < 1 || port > 65535 {
		return ProxyCandidate{}, false
	}
	proto := strings.ToLower(m[1])
	if proto == "" {
		proto = normalizedProtocol(defaultProto)
	}
	return ProxyCandidate{
		IP:       m[2],
		Port:     port,
		Proxy:    m[2] + ":" + strconv.Itoa(port),
		Protocol: proto,
	}, true
}

// ExtractProxies scans free-form text for every ip:port occurrence,
// assigning protocol to matches without an explicit prefix.
func ExtractProxies(text, protocol string) []ProxyCandidate {
	var out []ProxyCandidate
	for _, m := range proxyLinePattern.FindAllStringSubmatch(text, -1) {
		port, err := strconv.Atoi(m[3])
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		proto := strings.ToLower(m[1])
		if proto == "" {
			proto = normalizedProtocol(protocol)
		}
		out = append(out, ProxyCandidate{
			IP:       m[2],
			Port:     port,
			Proxy:    m[2] + ":" + strconv.Itoa(port),
			Protocol: proto,
		})
	}
	return out
}

// FetchFeed downloads one proxy list feed and extracts candidates,
// porting fast_validator.py::fetch_feed.
func FetchFeed(ctx context.Context, client *http.Client, feedURL, protocol string) []ProxyCandidate {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", petaniUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil
	}
	return ExtractProxies(string(body), protocol)
}

// HarvestRawCandidates ports fast_validator.py::harvest_raw_candidates: it
// fetches every feed for the requested protocols concurrently and dedupes by
// "protocol://ip:port". A nil protocols defaults to ["http", "socks5"] and
// a nil sources defaults to FastSources.
func HarvestRawCandidates(ctx context.Context, protocols []string, sources map[string][]string) []ProxyCandidate {
	if protocols == nil {
		protocols = []string{ProtocolHTTP, ProtocolSocks5}
	}
	if sources == nil {
		sources = FastSources
	}
	client := &http.Client{Timeout: 8 * time.Second}

	var mu sync.Mutex
	var batches [][]ProxyCandidate
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(16)
	for _, proto := range protocols {
		for _, feedURL := range sources[proto] {
			proto, feedURL := proto, feedURL
			g.Go(func() error {
				items := FetchFeed(gctx, client, feedURL, proto)
				mu.Lock()
				batches = append(batches, items)
				mu.Unlock()
				return nil
			})
		}
	}
	_ = g.Wait()

	seen := make(map[string]struct{})
	var deduped []ProxyCandidate
	for _, batch := range batches {
		for _, item := range batch {
			key := item.Protocol + "://" + item.Proxy
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			deduped = append(deduped, item)
		}
	}
	return deduped
}

// ValidatorOptions configures a Validator.
type ValidatorOptions struct {
	// MaxLatency is the acceptance ceiling; requests get twice this as a
	// hard timeout, mirroring aiohttp's total=2*max/connect=max split.
	MaxLatency time.Duration
	TestTarget string
}

// Validator is the Go equivalent of fast_validator.py's check_*_proxy
// functions plus validate_batch: a reusable, concurrency-safe latency
// checker. Use NewValidator, then CheckHTTPProxy / CheckSocksProxy /
// ValidateBatch.
type Validator struct {
	maxLatency time.Duration
	testTarget string
}

// NewValidator builds a Validator, applying fast_validator.py's defaults
// (350ms ceiling, DefaultTestTarget probe).
func NewValidator(opts ValidatorOptions) *Validator {
	if opts.MaxLatency <= 0 {
		opts.MaxLatency = 350 * time.Millisecond
	}
	if opts.TestTarget == "" {
		opts.TestTarget = DefaultTestTarget
	}
	return &Validator{maxLatency: opts.MaxLatency, testTarget: opts.TestTarget}
}

// CheckHTTPProxy ports check_http_proxy: one GET of the test target through
// an HTTP proxy. Non-200 responses, errors, and over-latency results yield
// an error (Python returns None).
func (v *Validator) CheckHTTPProxy(ctx context.Context, c ProxyCandidate) (*LiveProxy, error) {
	cc := c
	cc.Protocol = ProtocolHTTP
	return v.check(ctx, cc)
}

// CheckSocksProxy ports check_socks_proxy for socks4/socks5 candidates.
func (v *Validator) CheckSocksProxy(ctx context.Context, c ProxyCandidate) (*LiveProxy, error) {
	cc := c
	switch normalizedProtocol(cc.Protocol) {
	case ProtocolSocks4:
		cc.Protocol = ProtocolSocks4
	default:
		cc.Protocol = ProtocolSocks5
	}
	return v.check(ctx, cc)
}

func (v *Validator) check(ctx context.Context, c ProxyCandidate) (*LiveProxy, error) {
	proto := normalizedProtocol(c.Protocol)
	proxyAddr := c.Address()
	tr, err := transportForProxy(proto, proxyAddr, v.maxLatency)
	if err != nil {
		return nil, err
	}
	// Python: total timeout = 2*max_latency, connect timeout = max_latency.
	client := &http.Client{Transport: tr, Timeout: 2 * v.maxLatency}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.testTarget, nil)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: build request: %w", err)
	}
	req.Header.Set("User-Agent", petaniUserAgent)

	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: request via %s: %w", proxyAddr, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("petaniproxy: unexpected status %d via %s", resp.StatusCode, proxyAddr)
	}
	elapsedMs := int(time.Since(t0) / time.Millisecond)
	if elapsedMs > int(v.maxLatency/time.Millisecond) {
		return nil, fmt.Errorf("petaniproxy: %s too slow (%dms > %dms)", proxyAddr, elapsedMs, int(v.maxLatency/time.Millisecond))
	}
	ip, port := splitProxyAddr(proxyAddr)
	if c.IP != "" {
		ip = c.IP
	}
	if c.Port != 0 {
		port = c.Port
	}
	return &LiveProxy{
		IP:         ip,
		Port:       port,
		Proxy:      proxyAddr,
		Protocol:   proto,
		URL:        proto + "://" + proxyAddr,
		LatencySec: float64(elapsedMs) / 1000,
		LatencyMs:  elapsedMs,
		TestedAt:   time.Now().UTC(),
	}, nil
}

// ValidateBatch ports validate_batch: it checks candidates with at most
// maxWorkers in flight, collects live ones, calls onLive per hit, and stops
// early (cancelling the rest) once targetCount live proxies are found.
// targetCount <= 0 means no early stop. Results are sorted by latency.
func (v *Validator) ValidateBatch(ctx context.Context, candidates []ProxyCandidate, maxWorkers, targetCount int, onLive func(*LiveProxy)) []*LiveProxy {
	if maxWorkers <= 0 {
		maxWorkers = 200
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxWorkers)

	results := make(chan *LiveProxy, len(candidates))
	for _, c := range candidates {
		c := c
		g.Go(func() error {
			var (
				lp  *LiveProxy
				err error
			)
			if normalizedProtocol(c.Protocol) == ProtocolHTTP || c.Protocol == "" {
				lp, err = v.CheckHTTPProxy(gctx, c)
			} else {
				lp, err = v.CheckSocksProxy(gctx, c)
			}
			if err != nil || lp == nil {
				return nil
			}
			select {
			case results <- lp:
			case <-gctx.Done():
			}
			return nil
		})
	}
	go func() {
		g.Wait()
		close(results)
	}()

	var live []*LiveProxy
	for lp := range results {
		live = append(live, lp)
		if onLive != nil {
			onLive(lp)
		}
		if targetCount > 0 && len(live) >= targetCount {
			cancel()
			break
		}
	}
	_ = g.Wait()
	slices.SortFunc(live, func(a, b *LiveProxy) int { return a.LatencyMs - b.LatencyMs })
	return live
}

// SyncToSQLite ports fast_validator.py::sync_to_9router: it inserts live
// proxies as working rows into the proxyPools table using the same payload
// schema (name, proxyUrl, noProxy, type, strictProxy, lastTestedAt,
// lastError, latency seconds, egressIp). A missing/empty dbPath yields
// (0, nil), matching the Python early return.
func SyncToSQLite(proxies []*LiveProxy, dbPath string) (int, error) {
	if dbPath == "" {
		return 0, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return 0, nil
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return 0, fmt.Errorf("petaniproxy: open sqlite: %w", err)
	}
	defer db.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	inserted := 0
	for _, p := range proxies {
		payload := map[string]any{
			"name":         fmt.Sprintf("Fast Free (%s - %dms)", strings.ToUpper(p.Protocol), p.LatencyMs),
			"proxyUrl":     p.URL,
			"noProxy":      "",
			"type":         p.Protocol,
			"strictProxy":  false,
			"lastTestedAt": now,
			"lastError":    nil,
			"latency":      math.Round(float64(p.LatencyMs)/1000*100) / 100,
			"egressIp":     p.IP,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		_, err = db.Exec(
			`INSERT INTO proxyPools (id, isActive, testStatus, data, createdAt, updatedAt)
			 VALUES (?, 1, 'working', ?, ?, ?)`,
			uuid.NewString(), string(data), now, now,
		)
		if err == nil {
			inserted++
		}
	}
	return inserted, nil
}

// Find9RouterDB ports fast_validator.py::find_9router_db: it resolves the
// 9Router SQLite path from ROUTER_DB_PATH / BANSOS_ROUTER_DB / NINEROUTER_DB,
// falling back to the standard ~/.9router/db/data.sqlite location used by
// 9router-go. It returns "" when nothing exists.
func Find9RouterDB() string {
	for _, key := range []string{"ROUTER_DB_PATH", "BANSOS_ROUTER_DB", "NINEROUTER_DB"} {
		if p := os.Getenv(key); p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".9router", "db", "data.sqlite")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// HarvestOptions configures RunFastHarvester, mirroring
// run_fast_harvester_async's parameters. OutputFile empty disables file
// output; SyncDB with an empty DBPath resolves via Find9RouterDB.
type HarvestOptions struct {
	Protocols    []string
	Sources      map[string][]string
	MaxLatencyMs int
	TargetCount  int
	MaxWorkers   int
	BatchSize    int
	TestTarget   string
	SyncDB       bool
	DBPath       string
	OutputFile   string
	OnLive       func(*LiveProxy)
}

// RunFastHarvester ports run_fast_harvester_async: harvest candidates,
// validate them in batches until TargetCount live proxies are found, sort
// by latency, optionally write "proto://ip:port" lines to OutputFile and
// sync rows into SQLite. Progress reporting is via OnLive (the Python
// print() calls are intentionally not ported; this is a library).
func RunFastHarvester(ctx context.Context, opts HarvestOptions) ([]*LiveProxy, error) {
	if opts.MaxLatencyMs <= 0 {
		opts.MaxLatencyMs = 350
	}
	if opts.TargetCount <= 0 {
		opts.TargetCount = 15
	}
	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = 200
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 400
	}

	candidates := HarvestRawCandidates(ctx, opts.Protocols, opts.Sources)
	if len(candidates) == 0 {
		return nil, nil
	}

	v := NewValidator(ValidatorOptions{
		MaxLatency: time.Duration(opts.MaxLatencyMs) * time.Millisecond,
		TestTarget: opts.TestTarget,
	})
	var live []*LiveProxy
	for i := 0; i < len(candidates) && len(live) < opts.TargetCount; i += opts.BatchSize {
		end := min(i+opts.BatchSize, len(candidates))
		batch := v.ValidateBatch(ctx, candidates[i:end], opts.MaxWorkers, opts.TargetCount-len(live), opts.OnLive)
		live = append(live, batch...)
	}
	slices.SortFunc(live, func(a, b *LiveProxy) int { return a.LatencyMs - b.LatencyMs })

	if opts.OutputFile != "" && len(live) > 0 {
		var sb strings.Builder
		for _, p := range live {
			sb.WriteString(p.URL)
			sb.WriteByte('\n')
		}
		if err := os.MkdirAll(filepath.Dir(opts.OutputFile), 0o755); err != nil {
			return live, fmt.Errorf("petaniproxy: create output dir: %w", err)
		}
		if err := os.WriteFile(opts.OutputFile, []byte(sb.String()), 0o644); err != nil {
			return live, fmt.Errorf("petaniproxy: write output file: %w", err)
		}
	}

	if opts.SyncDB && len(live) > 0 {
		dbPath := opts.DBPath
		if dbPath == "" {
			dbPath = Find9RouterDB()
		}
		if _, err := SyncToSQLite(live, dbPath); err != nil {
			return live, fmt.Errorf("petaniproxy: sync to sqlite: %w", err)
		}
	}
	return live, nil
}
