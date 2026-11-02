package executor

import (
	"context"
	"crypto/md5" //nolint:gosec // canvas-hash parity with the reference fingerprint, not a security use
	"crypto/rand"
	"encoding/base64"
	json "encoding/json/v2"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Qwen Web's risk control (Alibaba Baxia) wants two tokens on every call: a
// bx-ua browser fingerprint and a bx-umidtoken device token. Neither needs a
// browser or an account: the guest lane accepts a *synthetic* bx-ua — a
// base64 JSON simulated fingerprint, the scheme the smanx/qwen2api wrapper
// documents and which this gateway verified live from its own egress on
// 2026-11-02 — and the umid token is minted by a public endpoint. This file
// is that generator, ported to Go rather than transliterated: same wire
// values, Go idioms for the randomness and the cache.

const (
	qwenWebBaseURL   = "https://chat.qwen.ai"
	qwenWebUMIDURL   = "https://sg-wum.alibaba.com/w/wu.json"
	qwenWebBXVersion = "2.5.37" // baxiaCommon version the live site served on 2026-11-02
	qwenWebSPAVer    = "0.3.28" // chat.qwen.ai SPA version captured the same day
	qwenWebUA        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
	qwenWebSecCHUA   = `"Chromium";v="150", "Google Chrome";v="150", "Not.A/Brand";v="24"`
	// The umid token is cached by the reference wrapper for ~25 minutes;
	// the same TTL bounds this cache (AGENTS.md §4.G: every cache needs a TTL).
	qwenWebUMIDTTL = 25 * time.Minute
)

// qwenWebFingerprint is the simulated browser fingerprint bx-ua carries.
// The field order matches the reference encoder: encoding/json/v2 marshals
// struct fields in declaration order, so the JSON key order on the wire is
// the one the reference produces.
type qwenWebFingerprint struct {
	Platform  string  `json:"p"`
	Language  string  `json:"l"`
	Cores     int     `json:"hc"`
	Memory    int     `json:"dm"`
	TZOffset  int     `json:"to"`
	ScrW      int     `json:"sw"`
	ScrH      int     `json:"sh"`
	ColorDep  int     `json:"cd"`
	PixRatio  float64 `json:"pr"`
	WebGL     string  `json:"wf"`
	Canvas    string  `json:"cf"`
	AudioFP   string  `json:"af"`
	Timestamp int64   `json:"ts"`
	Random    float64 `json:"r"`
}

var qwenWebRenderers = []string{
	"ANGLE (Intel, Intel(R) UHD Graphics 630, OpenGL 4.6)",
	"ANGLE (NVIDIA, NVIDIA GeForce GTX 1080, OpenGL 4.6)",
	"ANGLE (AMD, AMD Radeon RX 580, OpenGL 4.6)",
}

func qwenWebRandInt(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

func qwenWebRandFloat() float64 {
	v, err := rand.Int(rand.Reader, big.NewInt(1<<53))
	if err != nil {
		return 0.5
	}
	return float64(v.Int64()) / float64(1<<53)
}

// qwenWebBXUA builds one synthetic bx-ua token: the Baxia 2.5.36 scheme
// prefix ("2536!") plus base64 of the fingerprint JSON. A fresh fingerprint
// is generated per chat session, the way a fresh browser profile presents a
// fresh fingerprint; the values are randomised inside the same ranges the
// reference generator uses.
func qwenWebBXUA() string {
	seed := make([]byte, 32)
	_, _ = rand.Read(seed)
	canvasSum := md5.Sum(seed) //nolint:gosec // see const block note
	canvas := base64.StdEncoding.EncodeToString(canvasSum[:])
	if len(canvas) > 32 {
		canvas = canvas[:32]
	}
	renderer := qwenWebRenderers[qwenWebRandInt(len(qwenWebRenderers))]
	if len(renderer) > 20 {
		renderer = renderer[:20]
	}
	fp := qwenWebFingerprint{
		Platform:  "Win32",
		Language:  []string{"en-US", "zh-CN", "en-GB"}[qwenWebRandInt(3)],
		Cores:     4 + qwenWebRandInt(12),
		Memory:    []int{4, 8, 16, 32}[qwenWebRandInt(4)],
		TZOffset:  []int{-480, -300, 0, 60, 480}[qwenWebRandInt(5)],
		ScrW:      1920 + qwenWebRandInt(200),
		ScrH:      1080 + qwenWebRandInt(100),
		ColorDep:  24,
		PixRatio:  []float64{1, 1.25, 1.5, 2}[qwenWebRandInt(4)],
		WebGL:     renderer,
		Canvas:    canvas,
		AudioFP:   fmt.Sprintf("%.14f", 124.04347527516074+qwenWebRandFloat()*0.001),
		Timestamp: time.Now().UnixMilli(),
		Random:    qwenWebRandFloat(),
	}
	raw, err := json.Marshal(fp)
	if err != nil {
		return "2536!" + base64.StdEncoding.EncodeToString([]byte(`{"p":"Win32"}`))
	}
	return "2536!" + base64.StdEncoding.EncodeToString(raw)
}

// qwenWebUMIDCache holds the one shared umid token. Guest chats from this
// gateway present one device identity, so one token serves every session
// until it ages out or risk control refuses it.
var qwenWebUMIDCache struct {
	mu     sync.Mutex
	token  string
	expiry time.Time
}

var qwenWebUMIDPattern = regexp.MustCompile(`umx\.wu\('([^']+)'\)`)

// qwenWebUMIDToken returns the cached umid token, fetching a fresh one from
// the WUM endpoint when the cache is empty, expired, or force is set (the
// RGV587 retry path). When the endpoint cannot be reached, the documented
// fallback is a synthetic 'T2gA' + 40 random characters.
func qwenWebUMIDToken(ctx context.Context, client *http.Client, force bool) string {
	qwenWebUMIDCache.mu.Lock()
	defer qwenWebUMIDCache.mu.Unlock()
	if !force && qwenWebUMIDCache.token != "" && time.Now().Before(qwenWebUMIDCache.expiry) {
		return qwenWebUMIDCache.token
	}
	token := qwenWebFetchUMID(ctx, client)
	if token == "" {
		token = qwenWebFallbackUMID()
	}
	qwenWebUMIDCache.token = token
	qwenWebUMIDCache.expiry = time.Now().Add(qwenWebUMIDTTL)
	return token
}

func qwenWebFetchUMID(ctx context.Context, client *http.Client) string {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qwenWebUMIDURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", qwenWebUA)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ""
	}
	if m := qwenWebUMIDPattern.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	if etag := strings.Trim(resp.Header.Get("ETag"), `"`); strings.HasPrefix(etag, "T2gA") {
		return etag
	}
	return ""
}

func qwenWebFallbackUMID() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"
	var b strings.Builder
	b.WriteString("T2gA")
	for i := 0; i < 40; i++ {
		b.WriteByte(chars[qwenWebRandInt(len(chars))])
	}
	return b.String()
}

// qwenWebHeaders assembles the header set the site's own client sends,
// with a fresh x-request-id per call.
func qwenWebHeaders(bxUA, umid string) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/event-stream, */*")
	h.Set("Accept-Language", "id-ID,id;q=0.9,en-US;q=0.8,en;q=0.7")
	h.Set("bx-ua", bxUA)
	h.Set("bx-umidtoken", umid)
	h.Set("bx-v", qwenWebBXVersion)
	h.Set("Content-Type", "application/json")
	h.Set("Origin", qwenWebBaseURL)
	h.Set("Referer", qwenWebBaseURL+"/")
	h.Set("sec-ch-ua", qwenWebSecCHUA)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"Windows"`)
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "same-origin")
	h.Set("source", "h5")
	h.Set("User-Agent", qwenWebUA)
	h.Set("version", qwenWebSPAVer)
	h.Set("x-accel-buffering", "no")
	h.Set("x-request-id", qwenWebUUID())
	// The site sends a JS-Date-style timezone string for the browser's zone;
	// the gateway host runs in the operator's zone, so local time is the
	// honest value in the same format.
	now := time.Now()
	h.Set("timezone", now.Format("Mon Jan 02 2006 15:04:05")+" GMT"+now.Format("-0700"))
	return h
}

func qwenWebUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
