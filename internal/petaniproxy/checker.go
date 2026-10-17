package petaniproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// DefaultTestURL is the connectivity probe used by the checker, same as
// checker.py's DEFAULT_TEST_URL.
const DefaultTestURL = "https://api.ipify.org?format=json"

// Overridable endpoints (tests point these at local httptest servers).
var (
	hostIPEndpoints   = []string{"https://api.ipify.org?format=json", "https://icanhazip.com", "https://ifconfig.me/ip"}
	geoIPBatchURL     = "http://ip-api.com/batch"
	geoIPSingleURLFmt = "http://ip-api.com/json/%s?fields=country,countryCode,city,isp,status"
)

var hostIPCache struct {
	sync.Mutex
	done bool
	ip   string
}

// ResetHostIPCache clears the cached host IP, mainly for tests.
func ResetHostIPCache() {
	hostIPCache.Lock()
	defer hostIPCache.Unlock()
	hostIPCache.done = false
	hostIPCache.ip = ""
}

// GetHostIP returns this machine's external IP, cached after the first
// lookup (matching checker.py's _CACHED_HOST_IP). It tries several
// endpoints and returns "" when all fail.
func GetHostIP() string {
	hostIPCache.Lock()
	defer hostIPCache.Unlock()
	if !hostIPCache.done {
		hostIPCache.ip = fetchHostIP()
		hostIPCache.done = true
	}
	return hostIPCache.ip
}

func fetchHostIP() string {
	client := &http.Client{Timeout: 3500 * time.Millisecond}
	for _, endpoint := range hostIPEndpoints {
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", petaniUserAgent)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		status := resp.StatusCode
		resp.Body.Close()
		if err != nil || status < 200 || status >= 300 {
			continue
		}
		raw := strings.TrimSpace(string(body))
		var ip string
		if strings.HasPrefix(raw, "{") {
			var obj struct {
				IP string `json:"ip"`
			}
			if json.Unmarshal(body, &obj) == nil {
				ip = strings.TrimSpace(obj.IP)
			}
		} else {
			ip = raw
		}
		if ip != "" {
			return ip
		}
	}
	return ""
}

// proxyIndicatorHeaders mirrors checker.py's proxy_indicators set.
var proxyIndicatorHeaders = []string{
	"via", "x-forwarded-for", "forwarded", "x-real-ip",
	"proxy-connection", "x-proxy-id", "x-cache",
}

// ClassifyAnonymity ports checker.py::classify_anonymity:
//   - Transparent: the egress IP equals the host IP, or any response header
//     value leaks the host IP.
//   - Anonymous: host IP hidden, but a proxy-indicator header is present.
//   - Elite: host IP hidden and no proxy indicators.
func ClassifyAnonymity(headers http.Header, egressIP, hostIP string) string {
	if hostIP != "" && egressIP != "" && egressIP == hostIP {
		return AnonymityTransparent
	}
	foundProxyHeader := false
	for name, values := range headers {
		lower := strings.ToLower(name)
		for _, v := range values {
			if hostIP != "" && strings.Contains(v, hostIP) {
				return AnonymityTransparent
			}
		}
		for _, ind := range proxyIndicatorHeaders {
			if lower == ind {
				foundProxyHeader = true
				break
			}
		}
	}
	if foundProxyHeader {
		return AnonymityAnonymous
	}
	return AnonymityElite
}

// TestSingleProxy ports checker.py::test_single_proxy. It performs one GET
// of testURL through the candidate proxy, measures latency, extracts the
// egress IP from a {"ip": ...} JSON body, and classifies anonymity.
// It returns a non-nil *LiveProxy only for 2xx/3xx responses; any failure
// yields an error (Python returns None).
func TestSingleProxy(ctx context.Context, c ProxyCandidate, timeout time.Duration, testURL, hostIP string) (*LiveProxy, error) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if testURL == "" {
		testURL = DefaultTestURL
	}
	proto := normalizedProtocol(c.Protocol)
	proxyAddr := c.Address()
	tr, err := transportForProxy(proto, proxyAddr, timeout)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: tr, Timeout: timeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: build request: %w", err)
	}
	req.Header.Set("User-Agent", petaniUserAgent)

	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: request via %s: %w", proxyAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("petaniproxy: unexpected status %d via %s", resp.StatusCode, proxyAddr)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("petaniproxy: read body: %w", err)
	}
	elapsed := time.Since(t0)
	latencyMs := int(elapsed / time.Millisecond)

	egressIP := c.IP
	var obj struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(body, &obj) == nil && strings.TrimSpace(obj.IP) != "" {
		egressIP = strings.TrimSpace(obj.IP)
	}

	ip, port := splitProxyAddr(proxyAddr)
	if c.IP != "" {
		ip = c.IP
	}
	if c.Port != 0 {
		port = c.Port
	}
	geo := UnknownGeo()
	return &LiveProxy{
		IP:          ip,
		Port:        port,
		Proxy:       proxyAddr,
		Protocol:    proto,
		URL:         proto + "://" + proxyAddr,
		LatencySec:  math.Round(elapsed.Seconds()*1000) / 1000,
		LatencyMs:   latencyMs,
		EgressIP:    egressIP,
		Anonymity:   ClassifyAnonymity(resp.Header, egressIP, hostIP),
		Country:     geo.Country,
		CountryCode: geo.CountryCode,
		City:        geo.City,
		ISP:         geo.ISP,
		TestedAt:    time.Now().UTC(),
	}, nil
}

// BatchEnrichGeoIP ports checker.py::batch_enrich_geoip: it POSTs IPs in
// chunks of 100 to the ip-api.com batch endpoint and fills geo fields on
// successful lookups. Failures are swallowed per chunk, like the Python.
func BatchEnrichGeoIP(ctx context.Context, live []*LiveProxy) {
	if len(live) == 0 {
		return
	}
	byIP := make(map[string]*LiveProxy, len(live))
	var ips []string
	for _, p := range live {
		if _, ok := byIP[p.IP]; !ok {
			byIP[p.IP] = p
			ips = append(ips, p.IP)
		}
	}
	client := &http.Client{Timeout: 6 * time.Second}
	for i := 0; i < len(ips); i += 100 {
		chunk := ips[i:min(i+100, len(ips))]
		payload, err := json.Marshal(chunk)
		if err != nil {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, geoIPBatchURL, bytes.NewReader(payload))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", petaniUserAgent)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var items []struct {
			Query       string `json:"query"`
			Status      string `json:"status"`
			Country     string `json:"country"`
			CountryCode string `json:"countryCode"`
			City        string `json:"city"`
			ISP         string `json:"isp"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&items)
		resp.Body.Close()
		if decodeErr != nil || resp.StatusCode != 200 {
			continue
		}
		for _, it := range items {
			if p, ok := byIP[it.Query]; ok && it.Status == "success" {
				p.SetGeo(GeoInfo{Country: it.Country, CountryCode: it.CountryCode, City: it.City, ISP: it.ISP})
			}
		}
	}
}

// lookupGeoIP fetches geo data for one IP (used by the country filter).
func lookupGeoIP(ctx context.Context, ip string) GeoInfo {
	geo := UnknownGeo()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(geoIPSingleURLFmt, ip), nil)
	if err != nil {
		return geo
	}
	req.Header.Set("User-Agent", petaniUserAgent)
	resp, err := (&http.Client{Timeout: 2500 * time.Millisecond}).Do(req)
	if err != nil {
		return geo
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return geo
	}
	var item struct {
		Status      string `json:"status"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		City        string `json:"city"`
		ISP         string `json:"isp"`
	}
	if json.NewDecoder(resp.Body).Decode(&item) != nil || item.Status != "success" {
		return geo
	}
	return GeoInfo{Country: item.Country, CountryCode: item.CountryCode, City: item.City, ISP: item.ISP}
}

// CheckPoolOptions mirrors checker.py::check_proxies_pool parameters.
// HostIP overrides auto-detection (empty = call GetHostIP()); tests set it
// to avoid real network lookups.
type CheckPoolOptions struct {
	MaxCheck        int
	TargetAlive     int
	Timeout         time.Duration
	MaxWorkers      int
	CountryFilter   string
	AnonymityFilter string
	TestURL         string
	HostIP          string
	OnLive          func(live *LiveProxy, aliveCount, target int)
}

func (o *CheckPoolOptions) withDefaults() CheckPoolOptions {
	out := *o
	if out.MaxCheck <= 0 {
		out.MaxCheck = 300
	}
	if out.TargetAlive <= 0 {
		out.TargetAlive = 20
	}
	if out.Timeout <= 0 {
		out.Timeout = 3 * time.Second
	}
	if out.MaxWorkers <= 0 {
		out.MaxWorkers = 60
	}
	if out.TestURL == "" {
		out.TestURL = DefaultTestURL
	}
	return out
}

// CheckProxiesPool ports checker.py::check_proxies_pool: it checks up to
// MaxCheck candidates concurrently (at most MaxWorkers in flight), applies
// the anonymity/country filters, invokes OnLive per accepted proxy, and
// stops early once TargetAlive proxies are collected (remaining checks are
// cancelled, mirroring future.cancel()). The result is sorted by latency
// ascending and batch-enriched with GeoIP unless a country filter was used.
func CheckProxiesPool(ctx context.Context, candidates []ProxyCandidate, opts CheckPoolOptions) []*LiveProxy {
	opts = opts.withDefaults()
	toTest := candidates
	if len(toTest) > opts.MaxCheck {
		toTest = toTest[:opts.MaxCheck]
	}
	hostIP := opts.HostIP
	if hostIP == "" {
		hostIP = GetHostIP()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.MaxWorkers)

	results := make(chan *LiveProxy, len(toTest))
	for _, c := range toTest {
		c := c
		g.Go(func() error {
			lp, err := TestSingleProxy(gctx, c, opts.Timeout, opts.TestURL, hostIP)
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

	var alive []*LiveProxy
	anonFilter := strings.ToLower(opts.AnonymityFilter)
	for lp := range results {
		if anonFilter != "" && anonFilter != "all" && !strings.EqualFold(lp.Anonymity, anonFilter) {
			continue
		}
		if opts.CountryFilter != "" {
			geo := lookupGeoIP(ctx, lp.IP)
			lp.SetGeo(geo)
			cUpper := strings.ToUpper(opts.CountryFilter)
			if lp.CountryCode != cUpper && strings.ToUpper(lp.Country) != cUpper {
				continue
			}
		}
		alive = append(alive, lp)
		if opts.OnLive != nil {
			opts.OnLive(lp, len(alive), opts.TargetAlive)
		}
		if len(alive) >= opts.TargetAlive {
			cancel()
			break
		}
	}
	_ = g.Wait() // settle workers after early cancel

	if len(alive) > 0 && opts.CountryFilter == "" {
		BatchEnrichGeoIP(context.WithoutCancel(ctx), alive)
	}
	slices.SortFunc(alive, func(a, b *LiveProxy) int { return a.LatencyMs - b.LatencyMs })
	return alive
}
