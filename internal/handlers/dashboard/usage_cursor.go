package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strings"
	"time"

	json "encoding/json/v2"
)

// Cursor IDE quota via DashboardService Connect JSON on api2.cursor.sh.
// Port of open-sse/services/usage/cursor.js (decolua/9router#4689):
//
//   - Pro / Team / Ultra: POST GetCurrentPeriodUsage (spend in USD cents).
//   - Enterprise request buckets: GET /auth/usage as fallback.
//   - Plan label: POST GetPlanInfo (fail-open).
//
// Endpoint vars (not consts) so tests can point them at local servers,
// following the antigravityQuotaBaseURL pattern in this package.
var (
	cursorUsagePeriodURL = "https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	cursorUsagePlanURL   = "https://api2.cursor.sh/aiserver.v1.DashboardService/GetPlanInfo"
	cursorUsageAuthURL   = "https://api2.cursor.sh/auth/usage"
)

// Headers below mirror buildCursorHeaders in open-sse/utils/cursorChecksum.js
// (clientVersion 3.12.17, connect-es/1.6.1 UA). Only the usage call overrides
// content-type to application/json; the chat executor keeps connect+proto.
const (
	cursorClientVersion = "3.12.17"
	cursorClientCommit  = "0fb762053c34788bb7760d5673f8a6d4c8589d50"
)

// cursorCleanToken strips the optional "prefix::token" envelope. Both the
// usage fetcher and the /auth/usage fallback need the bare token.
func cursorCleanToken(token string) string {
	if i := strings.Index(token, "::"); i >= 0 {
		return token[i+2:]
	}
	return token
}

// cursorHashedHex is generateHashed64Hex: sha256 hex of input+salt.
func cursorHashedHex(input, salt string) string {
	sum := sha256.Sum256([]byte(input + salt))
	return hex.EncodeToString(sum[:])
}

// cursorUUIDv5 returns the RFC 4122 v5 (SHA-1, DNS namespace) UUID for name.
// Stdlib only — this is what upstream's uuidv5(token, DNS) produces for the
// x-session-id header.
func cursorUUIDv5(name string) string {
	var dnsNS = [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.New()
	h.Write(dnsNS[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)[:16]
	sum[6] = sum[6]&0x0f | 0x50 // version 5
	sum[8] = sum[8]&0x3f | 0x80 // variant RFC4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// cursorUUIDv4 returns a random v4 UUID for the per-request headers.
func cursorUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// cursorChecksum is the Jyh cipher obfuscation from cursorChecksum.js:
// 6 big-endian bytes of floor(now_ms/1e6), each XOR-chained from key 165,
// base64url-encoded without padding, with the machine id appended.
func cursorChecksum(machineID string, now time.Time) string {
	ts := now.UnixMilli() / 1e6
	var b [6]byte
	for i := 5; i >= 0; i-- {
		b[i] = byte(ts & 0xFF)
		ts >>= 8
	}
	t := 165
	for i := 0; i < len(b); i++ {
		b[i] = byte((int(b[i]^byte(t)) + i%256) & 0xFF)
		t = int(b[i])
	}
	return base64.RawURLEncoding.EncodeToString(b[:]) + machineID
}

func cursorClientOS() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return "linux"
	}
}

func cursorClientArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x64"
}

// cursorConnectHeaders builds the Connect JSON headers for the usage calls.
func cursorConnectHeaders(accessToken, machineID string, now time.Time) map[string]string {
	clean := cursorCleanToken(accessToken)
	effectiveMachineID := machineID
	if effectiveMachineID == "" {
		effectiveMachineID = cursorHashedHex(clean, "machineId")
	}
	return map[string]string{
		"Authorization":            "Bearer " + clean,
		"connect-accept-encoding":  "gzip",
		"connect-protocol-version": "1",
		"content-type":             "application/json",
		"accept":                   "application/json",
		"user-agent":               "connect-es/1.6.1",
		"x-amzn-trace-id":          "Root=" + cursorUUIDv4(),
		"x-client-key":             cursorHashedHex(clean, ""),
		"x-cursor-checksum":        cursorChecksum(effectiveMachineID, now),
		"x-cursor-client-version":  cursorClientVersion,
		"x-cursor-client-commit":   cursorClientCommit,
		"x-cursor-client-type":     "ide",
		"x-cursor-client-os":       cursorClientOS(),
		"x-cursor-client-arch":     cursorClientArch(),
		"x-cursor-client-device-type": "desktop",
		"x-cursor-config-version":  cursorUUIDv4(),
		"x-cursor-timezone":        "UTC",
		"x-ghost-mode":             "true",
		"x-request-id":             cursorUUIDv4(),
		"x-session-id":             cursorUUIDv5(clean),
	}
}

func cursorCentsToUSD(cents any) float64 {
	return math.Round(usageNum(cents, 0)/100*100) / 100
}

// cursorRemainingFromPercent mirrors remainingFromPercentUsed: clamp to [0,100].
func cursorRemainingFromPercent(percentUsed float64) float64 {
	return math.Max(0, 100-math.Min(100, math.Max(0, percentUsed)))
}

// cursorAddPlanSpendQuotas maps planUsage (USD cents) onto quota rows.
// Returns true when at least one row was added.
func cursorAddPlanSpendQuotas(quotas map[string]any, data map[string]any) bool {
	planUsage, _ := data["planUsage"].(map[string]any)
	if planUsage == nil {
		return false
	}
	before := len(quotas)
	limitCents := usageNum(planUsage["limit"], 0)
	usedCents := usageNum(firstNonNil(planUsage["includedSpend"], planUsage["totalSpend"]), 0)
	remainingCents := usageNum(planUsage["remaining"], math.Max(0, limitCents-usedCents))
	resetAt := usageResetTime(data["billingCycleEnd"])

	if limitCents > 0 {
		total := cursorCentsToUSD(limitCents)
		used := cursorCentsToUSD(usedCents)
		remainingUSD := cursorCentsToUSD(remainingCents)
		remainingPct := 0.0
		if planUsage["totalPercentUsed"] != nil {
			remainingPct = cursorRemainingFromPercent(usageNum(planUsage["totalPercentUsed"], 0))
		} else if total > 0 {
			remainingPct = remainingUSD / total * 100
		}
		q := usageQuota(used, total, resetAt)
		q["remainingPercentage"] = remainingPct
		quotas["Plan (USD)"] = q
	} else if planUsage["totalPercentUsed"] != nil {
		used := usageNum(planUsage["totalPercentUsed"], 0)
		q := usageQuota(used, 100, resetAt)
		q["remainingPercentage"] = cursorRemainingFromPercent(used)
		quotas["Included usage"] = q
	}
	if planUsage["autoPercentUsed"] != nil {
		used := usageNum(planUsage["autoPercentUsed"], 0)
		q := usageQuota(used, 100, resetAt)
		q["remainingPercentage"] = cursorRemainingFromPercent(used)
		quotas["Auto models (%)"] = q
	}
	if planUsage["apiPercentUsed"] != nil {
		used := usageNum(planUsage["apiPercentUsed"], 0)
		q := usageQuota(used, 100, resetAt)
		q["remainingPercentage"] = cursorRemainingFromPercent(used)
		quotas["Named models (%)"] = q
	}
	return len(quotas) > before
}

// cursorAddOnDemandQuota maps spendLimitUsage: personal bucket wins over the
// team pooled bucket, mirroring upstream's early return.
func cursorAddOnDemandQuota(quotas map[string]any, data map[string]any) {
	spend, _ := data["spendLimitUsage"].(map[string]any)
	if spend == nil {
		return
	}
	resetAt := usageResetTime(data["billingCycleEnd"])
	if individualLimit := usageNum(spend["individualLimit"], 0); individualLimit > 0 {
		used := cursorCentsToUSD(spend["individualUsed"])
		total := cursorCentsToUSD(individualLimit)
		remainingUSD := cursorCentsToUSD(firstNonNil(spend["individualRemaining"],
			math.Max(0, individualLimit-usageNum(spend["individualUsed"], 0))))
		q := usageQuota(used, total, resetAt)
		if total > 0 {
			q["remainingPercentage"] = remainingUSD / total * 100
		} else {
			q["remainingPercentage"] = 0.0
		}
		quotas["On-demand (USD)"] = q
		return
	}
	if pooledLimit := usageNum(spend["pooledLimit"], 0); pooledLimit > 0 {
		used := cursorCentsToUSD(spend["pooledUsed"])
		total := cursorCentsToUSD(pooledLimit)
		remainingUSD := cursorCentsToUSD(firstNonNil(spend["pooledRemaining"],
			math.Max(0, pooledLimit-usageNum(spend["pooledUsed"], 0))))
		q := usageQuota(used, total, resetAt)
		if total > 0 {
			q["remainingPercentage"] = remainingUSD / total * 100
		} else {
			q["remainingPercentage"] = 0.0
		}
		quotas["Team on-demand (USD)"] = q
	}
}

// cursorAddAuthUsageQuotas maps /auth/usage per-key request buckets.
func cursorAddAuthUsageQuotas(quotas map[string]any, data map[string]any) bool {
	if data == nil {
		return false
	}
	resetAt := usageResetTime(data["startOfMonth"])
	added := false
	for key, bucket := range data {
		bm, _ := bucket.(map[string]any)
		if bm == nil {
			continue
		}
		total := usageNum(bm["maxRequestUsage"], 0)
		if total <= 0 {
			continue
		}
		used := usageNum(bm["numRequests"], 0)
		q := usageQuota(used, total, resetAt)
		q["remainingPercentage"] = math.Max(0, (total-used)/total*100)
		quotas[key+" requests"] = q
		added = true
	}
	return added
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func cursorPostConnect(ctx context.Context, rawURL, token, machineID string) (int, []byte, error) {
	body, _ := json.Marshal(map[string]any{})
	status, _, out, err := usageDo(ctx, http.MethodPost, rawURL, cursorConnectHeaders(token, machineID, time.Now()), body)
	return status, out, err
}

// fetchCursorUsage reads live Cursor plan spend + request buckets.
func fetchCursorUsage(ctx context.Context, accessToken string, psd map[string]any) usageResult {
	if strings.TrimSpace(accessToken) == "" {
		return usageResult{message: "Cursor access token not available. Reconnect Cursor to view quota."}
	}
	token := strings.TrimSpace(accessToken)
	machineID := psdStr(psd, "machineId")

	status, out, err := cursorPostConnect(ctx, cursorUsagePeriodURL, token, machineID)
	if err != nil {
		return usageResult{message: fmt.Sprintf("Cursor error: %v", err)}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return usageResult{plan: "Cursor", message: "Cursor authentication failed. Reconnect the Cursor provider."}
	}

	quotas := map[string]any{}
	var periodData map[string]any
	periodOK := status >= 200 && status < 300
	if periodOK {
		periodData = usageJSON(out)
		cursorAddPlanSpendQuotas(quotas, periodData)
		cursorAddOnDemandQuota(quotas, periodData)
	}

	if len(quotas) == 0 && cursorUsageAuthURL != "" {
		authHeaders := map[string]string{
			"Authorization": "Bearer " + cursorCleanToken(token),
			"Accept":        "application/json",
		}
		if aStatus, _, aOut, aErr := usageDo(ctx, http.MethodGet, cursorUsageAuthURL, authHeaders, nil); aErr == nil && aStatus >= 200 && aStatus < 300 {
			cursorAddAuthUsageQuotas(quotas, usageJSON(aOut))
		} else if !periodOK {
			msg := fmt.Sprintf("Cursor usage API error (%d)", status)
			if text := strings.TrimSpace(string(bytes.TrimSpace(out))); text != "" {
				if len(text) > 120 {
					text = text[:120]
				}
				msg += ": " + text
			}
			return usageResult{plan: "Cursor", message: msg}
		}
	}

	if len(quotas) == 0 {
		fallback := "Cursor connected. No quota data returned for this plan."
		if periodData != nil {
			if dm, _ := periodData["displayMessage"].(string); strings.TrimSpace(dm) != "" {
				fallback = dm
			}
		}
		return usageResult{plan: "Cursor", message: fallback}
	}

	plan := "Cursor"
	if pStatus, pData, pErr := usagePost(ctx, cursorUsagePlanURL, map[string]any{}, cursorConnectHeaders(token, machineID, time.Now())); pErr == nil && pStatus >= 200 && pStatus < 300 {
		if info, _ := pData["planInfo"].(map[string]any); info != nil {
			if name, _ := info["planName"].(string); strings.TrimSpace(name) != "" {
				plan = "Cursor " + strings.TrimSpace(name)
			}
		}
	}
	return usageResult{plan: plan, quotas: quotas}
}
