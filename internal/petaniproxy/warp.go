// Port of petani-proxy/core/warp_generator.py (PetaniProxy v1.1).
//
// Registers free Cloudflare WARP WireGuard accounts via the official
// Cloudflare WARP REST API and renders them as WireGuard .conf and
// sing-box configs, with an optional sync into a 9Router SQLite proxyPools table.
package petaniproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/curve25519"
	_ "modernc.org/sqlite"
)

const (
	// WarpRegAPI is the public Cloudflare WARP registration endpoint.
	WarpRegAPI = "https://api.cloudflareclient.com/v0a2158/reg"
	// WarpUserAgent mirrors the official 1.1.1.1 client UA used by petani-proxy.
	WarpUserAgent = "okhttp/3.12.1"
	// DefaultWarpLocalPort is the mixed SOCKS5/HTTP inbound port in sing-box configs.
	DefaultWarpLocalPort = 10808
	// DefaultWarpDNS is the DNS set on generated WARP profiles.
	DefaultWarpDNS = "1.1.1.1, 1.0.0.1"
)

const (
	defaultWarpPeerPublicKey = "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo="
	defaultWarpEndpoint      = "162.159.193.10:2408"
	defaultWarpV4Address     = "172.16.0.2/32"
	defaultWarpEgressLabel   = "Cloudflare WARP Edge"
	warpNamePrefix           = "Cloudflare WARP"
	warpConfName             = "warp.conf"
	warpSingboxName          = "warp_singbox.json"
)

// warpHTTPClient is the shared client for WARP registration (12s timeout, mirroring urllib timeout=12).
var warpHTTPClient = &http.Client{Timeout: 12 * time.Second}

// WarpProfile is a registered Cloudflare WARP WireGuard account.
type WarpProfile struct {
	AccountID     string `json:"account_id"`
	Token         string `json:"token"`
	PrivateKey    string `json:"private_key"`
	PublicKey     string `json:"public_key"`
	PeerPublicKey string `json:"peer_public_key"`
	Endpoint      string `json:"endpoint"`
	V4Address     string `json:"v4_address"`
	V6Address     string `json:"v6_address"`
	DNS           string `json:"dns"`
	CreatedAt     string `json:"created_at"`
}

// GenerateWarpX25519Keypair generates a fresh X25519 keypair, base64-encoded
// (mirrors generate_x25519_keypair; fails only if crypto/rand fails).
func GenerateWarpX25519Keypair() (privB64, pubB64 string, err error) {
	var priv [32]byte
	if _, err = rand.Read(priv[:]); err != nil {
		return "", "", fmt.Errorf("GenerateWarpX25519Keypair: %w", err)
	}
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return "", "", fmt.Errorf("GenerateWarpX25519Keypair: %w", err)
	}
	return base64.StdEncoding.EncodeToString(priv[:]),
		base64.StdEncoding.EncodeToString(pub), nil
}

type warpRegRequest struct {
	Key          string `json:"key"`
	InstallID    string `json:"install_id"`
	FcmToken     string `json:"fcm_token"`
	TOS          string `json:"tos"`
	Model        string `json:"model"`
	SerialNumber string `json:"serial_number"`
	Locale       string `json:"locale"`
}

type warpRegResponse struct {
	ID     string `json:"id"`
	Token  string `json:"token"`
	Config struct {
		Peers []struct {
			PublicKey string `json:"public_key"`
			Endpoint  struct {
				Host string `json:"host"`
			} `json:"endpoint"`
		} `json:"peers"`
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
	} `json:"config"`
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// RegisterWarpAccount registers a fresh WARP profile against the real Cloudflare API.
func RegisterWarpAccount(ctx context.Context) (*WarpProfile, error) {
	return RegisterWarpAccountToURL(ctx, nil, WarpRegAPI)
}

// RegisterWarpAccountToURL is RegisterWarpAccount with an injectable client and
// API URL, so tests can point it at an httptest server instead of Cloudflare.
func RegisterWarpAccountToURL(ctx context.Context, client *http.Client, apiURL string) (*WarpProfile, error) {
	privB64, pubB64, err := GenerateWarpX25519Keypair()
	if err != nil {
		return nil, err
	}
	// Matches Python strftime("%Y-%m-%dT%H:%M:%S.000+00:00") exactly.
	nowISO := time.Now().UTC().Format("2006-01-02T15:04:05.000+00:00")

	body, err := json.Marshal(warpRegRequest{
		Key: pubB64, TOS: nowISO, Model: "PC", Locale: "en_US",
	})
	if err != nil {
		return nil, fmt.Errorf("RegisterWarpAccountToURL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("RegisterWarpAccountToURL: %w", err)
	}
	req.Header.Set("User-Agent", WarpUserAgent)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")

	c := client
	if c == nil {
		c = warpHTTPClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("RegisterWarpAccountToURL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("RegisterWarpAccountToURL: unexpected status %d", resp.StatusCode)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("RegisterWarpAccountToURL: %w", err)
	}
	var wr warpRegResponse
	if err := json.Unmarshal(respBody, &wr); err != nil {
		return nil, fmt.Errorf("RegisterWarpAccountToURL: %w", err)
	}

	peerPub, endpoint := defaultWarpPeerPublicKey, defaultWarpEndpoint
	if len(wr.Config.Peers) > 0 {
		peerPub = orDefault(wr.Config.Peers[0].PublicKey, defaultWarpPeerPublicKey)
		endpoint = orDefault(wr.Config.Peers[0].Endpoint.Host, defaultWarpEndpoint)
	}
	return &WarpProfile{
		AccountID:     wr.ID,
		Token:         wr.Token,
		PrivateKey:    privB64,
		PublicKey:     pubB64,
		PeerPublicKey: peerPub,
		Endpoint:      endpoint,
		V4Address:     orDefault(wr.Config.Interface.Addresses.V4, defaultWarpV4Address),
		V6Address:     wr.Config.Interface.Addresses.V6, // optional, may be empty
		DNS:           DefaultWarpDNS,
		CreatedAt:     nowISO,
	}, nil
}

// BuildWarpWireguardConf formats a WARP profile into a standard WireGuard .conf
// (mirrors build_wireguard_conf byte-for-byte).
func BuildWarpWireguardConf(p *WarpProfile) string {
	addrs := []string{p.V4Address}
	if p.V6Address != "" {
		addrs = append(addrs, p.V6Address)
	}
	var sb strings.Builder
	sb.WriteString("[Interface]\n")
	fmt.Fprintf(&sb, "PrivateKey = %s\n", p.PrivateKey)
	fmt.Fprintf(&sb, "Address = %s\n", strings.Join(addrs, ", "))
	fmt.Fprintf(&sb, "DNS = %s\n\n", orDefault(p.DNS, "1.1.1.1"))
	sb.WriteString("[Peer]\n")
	fmt.Fprintf(&sb, "PublicKey = %s\n", p.PeerPublicKey)
	sb.WriteString("AllowedIPs = 0.0.0.0/0, ::/0\n")
	fmt.Fprintf(&sb, "Endpoint = %s\n", p.Endpoint)
	return sb.String()
}

// WarpSingboxConfig is the sing-box config shape produced by build_singbox_config.
type WarpSingboxConfig struct {
	Log       WarpSingboxLog        `json:"log"`
	Inbounds  []WarpSingboxInbound  `json:"inbounds"`
	Outbounds []WarpSingboxOutbound `json:"outbounds"`
}

// WarpSingboxLog is the "log" section of a sing-box config.
type WarpSingboxLog struct {
	Level string `json:"level"`
}

// WarpSingboxInbound is the mixed SOCKS5/HTTP inbound section.
type WarpSingboxInbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Listen     string `json:"listen"`
	ListenPort int    `json:"listen_port"`
}

// WarpSingboxOutbound is the wireguard outbound section.
type WarpSingboxOutbound struct {
	Type          string   `json:"type"`
	Tag           string   `json:"tag"`
	Server        string   `json:"server"`
	ServerPort    int      `json:"server_port"`
	LocalAddress  []string `json:"local_address"`
	PrivateKey    string   `json:"private_key"`
	PeerPublicKey string   `json:"peer_public_key"`
	Reserved      []int    `json:"reserved"`
}

// BuildWarpSingboxConfig formats a WARP profile into a sing-box mixed
// SOCKS5/HTTP inbound config (mirrors build_singbox_config).
func BuildWarpSingboxConfig(p *WarpProfile, localPort int) (*WarpSingboxConfig, error) {
	idx := strings.LastIndex(p.Endpoint, ":")
	if idx < 0 {
		return nil, fmt.Errorf("BuildWarpSingboxConfig: malformed endpoint %q", p.Endpoint)
	}
	port, err := strconv.Atoi(p.Endpoint[idx+1:])
	if err != nil {
		return nil, fmt.Errorf("BuildWarpSingboxConfig: malformed endpoint %q: %w", p.Endpoint, err)
	}
	localAddrs := []string{p.V4Address}
	if p.V6Address != "" {
		localAddrs = append(localAddrs, p.V6Address)
	}
	return &WarpSingboxConfig{
		Log: WarpSingboxLog{Level: "warn"},
		Inbounds: []WarpSingboxInbound{
			{Type: "mixed", Tag: "mixed-in", Listen: "127.0.0.1", ListenPort: localPort},
		},
		Outbounds: []WarpSingboxOutbound{
			{
				Type: "wireguard", Tag: "warp-out",
				Server: p.Endpoint[:idx], ServerPort: port,
				LocalAddress: localAddrs,
				PrivateKey:   p.PrivateKey, PeerPublicKey: p.PeerPublicKey,
				Reserved: []int{0, 0, 0},
			},
		},
	}, nil
}

// FindWarpRouterDB locates a 9Router SQLite database: env vars first
// (ROUTER_DB_PATH, BANSOS_ROUTER_DB, NINEROUTER_DB), then the petani-proxy
// sibling-tree candidates relative to baseDir. Returns "" when nothing exists.
func FindWarpRouterDB(baseDir string) string {
	for _, env := range []string{"ROUTER_DB_PATH", "BANSOS_ROUTER_DB", "NINEROUTER_DB"} {
		if p := os.Getenv(env); p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if baseDir == "" {
		return ""
	}
	for _, rel := range []string{
		"../eLrouter/data/db/data.sqlite",
		"../9router-mibp-version/data/db/data.sqlite",
		"../9router/data/db/data.sqlite",
		"../bansos-router/data/db/data.sqlite",
	} {
		c := filepath.Clean(filepath.Join(baseDir, rel))
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// SyncWarpTo9Router registers the local WARP endpoint as a 9Router proxyPools
// row (mirrors sync_warp_to_9router). Existing WARP rows are replaced.
// NOTE: fixes a Python bug where sqlite3.connect(db_path) ignored the resolved
// target_db, so sync only ever worked when db_path was passed explicitly.
func SyncWarpTo9Router(ctx context.Context, localPort int, dbPath string) error {
	target := dbPath
	if target == "" {
		target = FindWarpRouterDB("")
	}
	if target == "" {
		return fmt.Errorf("SyncWarpTo9Router: no 9Router database found (set ROUTER_DB_PATH)")
	}
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("SyncWarpTo9Router: %w", err)
	}

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", localPort)
	nowISO := time.Now().UTC().Format(time.RFC3339)

	data, err := json.Marshal(map[string]any{
		"name":         fmt.Sprintf("Cloudflare WARP Ultra-Fast (%s)", proxyURL),
		"proxyUrl":     proxyURL,
		"noProxy":      "localhost,127.0.0.1",
		"type":         "http",
		"strictProxy":  false,
		"lastTestedAt": nowISO,
		"lastError":    nil,
		"latency":      0.05,
		"egressIp":     defaultWarpEgressLabel,
	})
	if err != nil {
		return fmt.Errorf("SyncWarpTo9Router: %w", err)
	}

	db, err := sql.Open("sqlite", target)
	if err != nil {
		return fmt.Errorf("SyncWarpTo9Router: %w", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, "DELETE FROM proxyPools WHERE data LIKE '%"+warpNamePrefix+"%'"); err != nil {
		return fmt.Errorf("SyncWarpTo9Router: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO proxyPools (id, isActive, testStatus, data, createdAt, updatedAt)
		 VALUES (?, 1, 'working', ?, ?, ?)`,
		uuid.NewString(), string(data), nowISO, nowISO); err != nil {
		return fmt.Errorf("SyncWarpTo9Router: %w", err)
	}
	return nil
}

// WarpGenerateOptions controls GenerateAndSaveWarp.
// Note: Python defaults sync_db=True; here DB sync is explicit opt-in via SyncDB.
type WarpGenerateOptions struct {
	// OutputDir receives warp.conf and warp_singbox.json (default: ./output/warp).
	OutputDir string
	// LocalPort is the sing-box mixed inbound port (default DefaultWarpLocalPort).
	LocalPort int
	// SyncDB also registers the endpoint into a 9Router SQLite database.
	SyncDB bool
	// DBPath overrides the database lookup; "" resolves via FindWarpRouterDB.
	DBPath string
	// APIURL overrides the Cloudflare registration endpoint (tests only).
	APIURL string
	// Client overrides the HTTP client (tests only).
	Client *http.Client
}

// GenerateAndSaveWarp is the high-level runner (mirrors generate_and_save_warp):
// registers an account, writes warp.conf + warp_singbox.json, optionally syncs
// the endpoint into 9Router SQLite. A sync failure is non-fatal, matching Python.
func GenerateAndSaveWarp(ctx context.Context, opts WarpGenerateOptions) (*WarpProfile, error) {
	outputDir := opts.OutputDir
	if outputDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("GenerateAndSaveWarp: %w", err)
		}
		outputDir = filepath.Join(cwd, "output", "warp")
	}
	localPort := opts.LocalPort
	if localPort == 0 {
		localPort = DefaultWarpLocalPort
	}
	apiURL := opts.APIURL
	if apiURL == "" {
		apiURL = WarpRegAPI
	}

	profile, err := RegisterWarpAccountToURL(ctx, opts.Client, apiURL)
	if err != nil {
		return nil, fmt.Errorf("GenerateAndSaveWarp: %w", err)
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("GenerateAndSaveWarp: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, warpConfName),
		[]byte(BuildWarpWireguardConf(profile)), 0o644); err != nil {
		return nil, fmt.Errorf("GenerateAndSaveWarp: %w", err)
	}

	sbcfg, err := BuildWarpSingboxConfig(profile, localPort)
	if err != nil {
		return nil, fmt.Errorf("GenerateAndSaveWarp: %w", err)
	}
	sbJSON, err := json.MarshalIndent(sbcfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("GenerateAndSaveWarp: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, warpSingboxName), sbJSON, 0o644); err != nil {
		return nil, fmt.Errorf("GenerateAndSaveWarp: %w", err)
	}

	// Mirrors Python: a sync failure does not fail the run.
	if opts.SyncDB {
		_ = SyncWarpTo9Router(ctx, localPort, opts.DBPath)
	}
	return profile, nil
}
