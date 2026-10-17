// Package petaniproxy — multi-format exporter.
//
// Go port of petani-proxy/core/exporter.py: writes verified-alive proxies to
// TXT (raw, URL, elite, per-protocol), JSON (rich payload), CSV, and can
// optionally sync them into the 9Router SQLite proxyPools table.
package petaniproxy

import (
	"crypto/rand"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Exported file name constants, mirroring the Python exporter's outputs.
const (
	ExportFileAll    = "live_all.txt"
	ExportFileURLs   = "live_urls.txt"
	ExportFileElite  = "live_elite.txt"
	ExportFileJSON   = "proxies.json"
	ExportFileCSV    = "proxies.csv"
	ExportDefaultDir = "output"
)

// Export result map keys, matching the Python dict returned by
// export_all_formats.
const (
	ExportKeyAllTXT   = "all_txt"
	ExportKeyURLsTXT  = "urls_txt"
	ExportKeyEliteTXT = "elite_txt"
	ExportKeyJSON     = "json"
	ExportKeyCSV      = "csv"
	ExportKeyDB       = "9router_db"
)

// protocolsExported is the fixed per-protocol file set the Python exporter
// always writes, even when a protocol bucket is empty.
var protocolsExported = []string{"http", "socks4", "socks5"}

// ProxyExportPayload is the rich JSON document written to proxies.json.
type ProxyExportPayload struct {
	GeneratedAt time.Time      `json:"generated_at"`
	TotalAlive  int            `json:"total_alive"`
	Protocols   map[string]int `json:"protocols"`
	Anonymity   map[string]int `json:"anonymity"`
	Proxies     []PoolProxy    `json:"proxies"`
}

// ExportAllFormats writes the proxy set to outputDir in every format the
// Python exporter produces and returns a map of result keys to generated
// paths (same keys as the Python dict: "all_txt", "urls_txt", "elite_txt",
// "<proto>_txt", "json", "csv").
//
// An empty outputDir falls back to "output" relative to the working
// directory, mirroring the Python <repo>/output default.
//
// When db is non-nil, live proxies are also synced into the 9Router SQLite
// proxyPools table via SyncTo9RouterDB and the result map gains a
// "9router_db" key with the value "synced:<inserted>". The caller owns the
// *sql.DB (opened with its own driver, e.g. modernc.org/sqlite) — this
// package only imports database/sql, staying stdlib-only.
func ExportAllFormats(proxies []PoolProxy, outputDir string, db *sql.DB) (map[string]string, error) {
	if outputDir == "" {
		outputDir = ExportDefaultDir
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("petaniproxy.ExportAllFormats: mkdir %s: %w", outputDir, err)
	}
	generated := make(map[string]string)

	byProto := map[string][]string{"http": {}, "socks4": {}, "socks5": {}}
	byAnon := map[string]int{"Elite": 0, "Anonymous": 0, "Transparent": 0}
	var allRaw, allURLs, eliteURLs []string

	for _, p := range proxies {
		proto := normalizeProto(p.Protocol)
		raw := p.Key()
		proxyURL := proto + "://" + raw
		anon := p.Anonymity
		if anon == "" {
			anon = "Elite" // Python default: p.get("anonymity", "Elite")
		}

		allRaw = append(allRaw, raw)
		allURLs = append(allURLs, proxyURL)
		if bucket, ok := byProto[proto]; ok {
			byProto[proto] = append(bucket, raw)
		}
		byAnon[anon]++
		if anon == "Elite" {
			eliteURLs = append(eliteURLs, proxyURL)
		}
	}

	writeLines := func(name string, lines []string) (string, error) {
		path := filepath.Join(outputDir, name)
		if err := WriteProxyLines(path, lines); err != nil {
			return "", err
		}
		return path, nil
	}

	var err error
	if generated[ExportKeyAllTXT], err = writeLines(ExportFileAll, allRaw); err != nil {
		return nil, err
	}
	if generated[ExportKeyURLsTXT], err = writeLines(ExportFileURLs, allURLs); err != nil {
		return nil, err
	}
	if generated[ExportKeyEliteTXT], err = writeLines(ExportFileElite, eliteURLs); err != nil {
		return nil, err
	}
	for _, proto := range protocolsExported {
		key := proto + "_txt"
		if generated[key], err = writeLines("live_"+proto+".txt", byProto[proto]); err != nil {
			return nil, err
		}
	}

	jsonPath := filepath.Join(outputDir, ExportFileJSON)
	payload := ProxyExportPayload{
		GeneratedAt: time.Now().UTC(),
		TotalAlive:  len(proxies),
		Protocols: map[string]int{
			"http":   len(byProto["http"]),
			"socks4": len(byProto["socks4"]),
			"socks5": len(byProto["socks5"]),
		},
		Anonymity: byAnon,
		Proxies:   proxies,
	}
	if err := WriteProxyJSON(jsonPath, payload); err != nil {
		return nil, err
	}
	generated[ExportKeyJSON] = jsonPath

	csvPath := filepath.Join(outputDir, ExportFileCSV)
	if err := WriteProxyCSV(csvPath, proxies); err != nil {
		return nil, err
	}
	generated[ExportKeyCSV] = csvPath

	if db != nil {
		inserted, err := SyncTo9RouterDB(db, proxies, true)
		if err != nil {
			return nil, err
		}
		generated[ExportKeyDB] = fmt.Sprintf("synced:%d", inserted)
	}
	return generated, nil
}

// WriteProxyLines writes lines to path, one per line with a trailing newline
// when non-empty (an empty list yields an empty file, as in Python).
func WriteProxyLines(path string, lines []string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("petaniproxy.WriteProxyLines: create %s: %w", path, err)
	}
	defer f.Close()
	return WriteProxyLinesTo(f, lines)
}

// WriteProxyLinesTo streams lines to w for callers that already own the
// destination (files, buffers, network).
func WriteProxyLinesTo(w io.Writer, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	return err
}

// WriteProxyJSON marshals the export payload with 2-space indent, mirroring
// Python's json.dump(indent=2).
func WriteProxyJSON(path string, payload ProxyExportPayload) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("petaniproxy.WriteProxyJSON: marshal: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("petaniproxy.WriteProxyJSON: write %s: %w", path, err)
	}
	return nil
}

// proxyCSVHeader is the fixed column set of proxies.csv (extra proxy fields
// are ignored, mirroring csv.DictWriter(extrasaction="ignore")).
var proxyCSVHeader = []string{
	"protocol", "ip", "port", "proxy", "anonymity", "latency_ms",
	"country_code", "country", "city", "isp", "egress_ip",
}

// WriteProxyCSV writes the proxy list as CSV with the fixed header above.
func WriteProxyCSV(path string, proxies []PoolProxy) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("petaniproxy.WriteProxyCSV: create %s: %w", path, err)
	}
	defer f.Close()
	return WriteProxyCSVTo(f, proxies)
}

// WriteProxyCSVTo streams the CSV document to w.
func WriteProxyCSVTo(w io.Writer, proxies []PoolProxy) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(proxyCSVHeader); err != nil {
		return fmt.Errorf("petaniproxy.WriteProxyCSVTo: header: %w", err)
	}
	for _, p := range proxies {
		anon := p.Anonymity
		if anon == "" {
			anon = "Elite"
		}
		row := []string{
			normalizeProto(p.Protocol),
			p.IP,
			strconv.Itoa(p.Port),
			p.Key(),
			anon,
			strconv.FormatFloat(p.LatencyMs, 'f', -1, 64),
			p.CountryCode,
			p.Country,
			p.City,
			p.ISP,
			p.EgressIP,
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("petaniproxy.WriteProxyCSVTo: row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// nineRouterDeleteSQL mirrors the Python sync's cleanup: drop previously
// synced PetaniProxy/OmniProxy rows plus anything still 'unknown'.
const nineRouterDeleteSQL = `DELETE FROM proxyPools WHERE data LIKE '%PetaniProxy%' OR data LIKE '%OmniProxy%' OR testStatus = 'unknown'`

const nineRouterInsertSQL = `
INSERT INTO proxyPools (id, isActive, testStatus, data, createdAt, updatedAt)
VALUES (?, 1, 'working', ?, ?, ?)
`

// SyncTo9RouterDB inserts live proxies into the 9Router SQLite proxyPools
// table, one row per proxy with the same payload shape the Python exporter
// writes (name, proxyUrl, noProxy, type, strictProxy, lastTestedAt,
// lastError, latency, egressIp, country). When replace is true it first
// deletes rows previously synced by this harvester and any 'unknown' rows,
// exactly like the Python version. It returns the number of rows inserted.
func SyncTo9RouterDB(db *sql.DB, proxies []PoolProxy, replace bool) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("petaniproxy.SyncTo9RouterDB: nil db")
	}
	if replace {
		if _, err := db.Exec(nineRouterDeleteSQL); err != nil {
			return 0, fmt.Errorf("petaniproxy.SyncTo9RouterDB: cleanup: %w", err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	inserted := 0
	for _, p := range proxies {
		proto := normalizeProto(p.Protocol)
		egress := p.EgressIP
		if egress == "" {
			egress = p.IP
		}
		country := p.CountryCode
		if country == "" {
			country = "??"
		}
		latency := p.LatencySec
		if latency == 0 {
			latency = 1.0 // Python: p.get("latency_sec", 1.0)
		}
		payload := map[string]any{
			"name":         fmt.Sprintf("PetaniProxy [%s] %s (%s)", country, p.Key(), proto),
			"proxyUrl":     proto + "://" + p.Key(),
			"noProxy":      "",
			"type":         proto,
			"strictProxy":  false,
			"lastTestedAt": now,
			"lastError":    nil,
			"latency":      latency,
			"egressIp":     egress,
			"country":      country,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return inserted, fmt.Errorf("petaniproxy.SyncTo9RouterDB: marshal payload: %w", err)
		}
		id, err := newUUIDv4()
		if err != nil {
			return inserted, fmt.Errorf("petaniproxy.SyncTo9RouterDB: uuid: %w", err)
		}
		if _, err := db.Exec(nineRouterInsertSQL, id, string(data), now, now); err != nil {
			return inserted, fmt.Errorf("petaniproxy.SyncTo9RouterDB: insert: %w", err)
		}
		inserted++
	}
	return inserted, nil
}

// normalizeProto lowercases the protocol, defaulting to "http" like the
// Python p.get("protocol", "http").lower().
func normalizeProto(proto string) string {
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto == "" {
		return "http"
	}
	return proto
}

// newUUIDv4 generates a random v4 UUID using only the stdlib (the Python
// exporter uses uuid.uuid4; this keeps the package dependency-free).
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	hexed := hex.EncodeToString(b[:])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32], nil
}
