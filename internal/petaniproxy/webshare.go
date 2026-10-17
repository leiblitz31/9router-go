// Package petaniproxy is a Go port of the petani-proxy OmniProxy Harvester
// webshare hunter (core/webshare_hunter.py): a residential proxy hunter that
// registers Webshare accounts through a real Chromium browser with human-like
// behavior (Bezier-curve mouse movement, typing simulation with random
// delays) and harvests the free residential proxy list from the dashboard.
//
// Behavioral parity notes (Python -> Go):
//   - Browser automation used DrissionPage (Chromium via CDP). chromedp is NOT
//     a dependency of this module, so the browser surface is expressed as the
//     WebshareBrowser / WebshareElement interfaces. A production wiring adds
//     chromedp to go.mod and implements those interfaces (method docs say
//     exactly which chromedp call each method maps to).
//   - Audio captcha solving used speech_recognition (Google STT) on a
//     wav converted with pydub. Go has no equivalent stdlib pieces, so audio
//     bytes go to the WebshareCaptchaSolver interface; the bundled
//     WebshareStubCaptchaSolver always errors. Production needs an STT API
//     key (e.g. OpenAI Whisper) or a local faster-whisper sidecar.
//   - Proxy persistence used sqlite3 directly. Go expresses persistence as
//     the WebshareProxyStore interface with a WebshareMemoryStore
//     implementation. A production sqlite wiring can implement the same two
//     methods against modernc.org/sqlite (already in go.mod) using the
//     proxyPools schema documented on WebshareProxyPoolEntry.
//   - The optional paid CapSolver reCAPTCHA path is ported as plain HTTP
//     (createTask / getTaskResult polling) with the same 30x2s poll loop and
//     the same $0.003 minimum-balance gate for headless mode.
//   - Randomness uses math/rand (seeded per hunter); the Python used the
//     global random module. Delays go through WebshareSleeper so tests run
//     without real sleeping.
package petaniproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	randv1 "math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Constants: URLs, thresholds, timings (mirrors the Python module level)
// ---------------------------------------------------------------------------

// WebshareRegisterURL is the account registration page the hunter drives.
const WebshareRegisterURL = "https://proxy.webshare.io/register"

// WebshareHeadlessUserAgent mirrors the UA the Python forces in headless
// mode so reCAPTCHA does not throttle the audio challenge.
const WebshareHeadlessUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"

// WebshareDefaultChromiumArgs mirrors the ChromiumOptions flags from the Python.
var WebshareDefaultChromiumArgs = []string{
	"--disable-blink-features=AutomationControlled",
	"--no-sandbox",
	"--disable-dev-shm-usage",
}

// WebshareCapsolverMinBalanceUSD is the $0.003 gate from check_capsolver_balance:
// below it headless mode is disabled.
const WebshareCapsolverMinBalanceUSD = 0.003

// WebshareCapsolverMaxPolls and WebshareCapsolverPollInterval mirror the
// Python's `for _ in range(30): time.sleep(2)` task-result poll loop.
const WebshareCapsolverMaxPolls = 30

// WebshareCapsolverPollInterval mirrors the 2s sleep between getTaskResult polls.
const WebshareCapsolverPollInterval = 2 * time.Second

// WebshareFallbackEmailDomains is the verified fallback domain pool used when
// no custom domain, env var, config file, DuckMail or mail.tm yields a domain.
var WebshareFallbackEmailDomains = []string{
	"niceground.shop",
	"stoneground.shop",
	"lakeground.shop",
	"canvaspace.shop",
	"vercelspace.shop",
	"bananaspace.shop",
	"sunstarmoon.shop",
	"makesomestone.shop",
	"hubaiclass.org",
	"markaihub.shop",
	"uberip.com",
}

// Default hunt timings, mirroring the Python's hard-coded sleeps.
const (
	// WebshareDefaultLoginTimeout mirrors `while time.time() - start_time < 180`.
	WebshareDefaultLoginTimeout = 180 * time.Second
	// WebshareDefaultHarvestTimeout mirrors `while time.time() - poll_start < 60`.
	WebshareDefaultHarvestTimeout = 60 * time.Second
	// WebshareDefaultSolverInterval mirrors `if time.time() - last_attempt_time > 15`.
	WebshareDefaultSolverInterval = 15 * time.Second
	// WebshareDefaultCooldownRetry mirrors the 30s "Too many attempts" cooldown.
	WebshareDefaultCooldownRetry = 30 * time.Second
	// WebshareDefaultAccountGap mirrors the 3s rest between accounts.
	WebshareDefaultAccountGap = 3 * time.Second
	// WebshareDefaultFinalHold mirrors the 15s hold when no proxy was harvested.
	WebshareDefaultFinalHold = 15 * time.Second
)

// ---------------------------------------------------------------------------
// Embedded page scripts (ported verbatim from the Python run_js strings)
// ---------------------------------------------------------------------------

const webshareSitekeyJS = `
const el = document.querySelector('[data-sitekey]');
if (el) return el.getAttribute('data-sitekey');
const iframe = document.querySelector('iframe[src*="recaptcha"]');
if (iframe) {
    const match = iframe.src.match(/[?&]k=([^&]+)/);
    if (match) return match[1];
}
return '';`

const webshareTooManyAttemptsJS = `
const alert = Array.from(document.querySelectorAll('div, p, span')).find(el => el.innerText && el.innerText.includes('Too many attempts'));
return !!alert;`

const webshareDismissModalsJS = `
const buttons = Array.from(document.querySelectorAll('button'));
const btnGo = buttons.find(b => b.innerText.includes('Go To Proxy List'));
if (btnGo) btnGo.click();
const closeBtn = document.querySelector('button[aria-label="Close"], svg[data-testid="CloseIcon"]');
if (closeBtn) closeBtn.click();`

const webshareDownloadLinkJS = `
const input = document.querySelector('input[value*="proxy/list/download"]');
if (input) return input.value;
const anyInput = Array.from(document.querySelectorAll('input')).find(i => i.value && i.value.includes('/download/'));
return anyInput ? anyInput.value : '';`

// webshareTableRowsJS returns every table row as an array of trimmed cell
// texts. The Python returned ready-made proxy strings from JS; the Go port
// returns raw rows and parses them in ParseWebshareProxyTable so the parsing
// is unit-testable.
const webshareTableRowsJS = `
return Array.from(document.querySelectorAll('tr')).map(r => Array.from(r.querySelectorAll('td, th')).map(c => c.innerText.trim()));`

const webshareCheckboxJS = `
const chk = document.querySelector("input[type='checkbox'], input.PrivateSwitchBase-input");
if (chk && !chk.checked) { chk.click(); }`

const webshareAudioHasInputJS = `return !!document.getElementById("audio-response");`

const webshareAudioHeadphoneJS = `
const btn = document.getElementById('recaptcha-audio-button') ||
            document.querySelector('.rc-button-audio');
if (btn && btn.offsetParent !== null) {
    btn.click();
    return true;
}
return false;`

const webshareAudioPlayJS = `
const playBtn = Array.from(document.querySelectorAll('button')).find(b => b.innerText.includes('PLAY'));
if (playBtn) playBtn.click();`

const webshareAudioLimitJS = `
const el = document.querySelector('.rc-doscaptcha-header-text') ||
           Array.from(document.querySelectorAll('div, p')).find(e => e.innerText && e.innerText.includes('automated queries'));
return el ? el.innerText : '';`

const webshareAudioSourceJS = `
const src = document.getElementById('audio-source');
if (src && src.src) return src.src;
const a = document.querySelector('a.rc-audiochallenge-tdownload-link') ||
          document.querySelector('a[href*=".mp3"]');
return a ? a.href : '';`

const webshareAudioFocusClearJS = `
const inp = document.getElementById('audio-response');
if (inp) { inp.focus(); inp.value = ""; }`

const webshareAudioVerifyJS = `
const vbtn = document.getElementById('recaptcha-verify-button');
if (vbtn) {
    vbtn.click();
    return true;
}
return false;`

// ---------------------------------------------------------------------------
// Pure human-behavior math: Bezier mouse path, jitter, typing delays
// ---------------------------------------------------------------------------

// WebsharePoint is one cursor position on the simulated mouse path.
type WebsharePoint struct {
	X, Y float64
}

// WebshareQuadraticBezier evaluates the 1D quadratic Bezier curve
// (1-t)^2*p0 + 2*(1-t)*t*p1 + t^2*p2, mirroring simulate_human_mouse.
func WebshareQuadraticBezier(t, p0, p1, p2 float64) float64 {
	u := 1 - t
	return u*u*p0 + 2*u*t*p1 + t*t*p2
}

// WebshareMousePath returns steps+1 cursor positions from (startX,startY) to
// (endX,endY) along the quadratic Bezier through the control point,
// mirroring the `for i in range(steps + 1)` loop in simulate_human_mouse.
func WebshareMousePath(startX, startY, ctrlX, ctrlY, endX, endY float64, steps int) []WebsharePoint {
	if steps < 1 {
		steps = 1
	}
	pts := make([]WebsharePoint, 0, steps+1)
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		pts = append(pts, WebsharePoint{
			X: WebshareQuadraticBezier(t, startX, ctrlX, endX),
			Y: WebshareQuadraticBezier(t, startY, ctrlY, endY),
		})
	}
	return pts
}

// WebshareUniformFloat mirrors random.uniform(min, max).
func WebshareUniformFloat(r *randv1.Rand, min, max float64) float64 {
	return min + r.Float64()*(max-min)
}

// WebshareAudioListenWait mirrors `listen_wait = max(4.0, duration_sec + 1.5)`.
func WebshareAudioListenWait(durationSec float64) float64 {
	return math.Max(4.0, durationSec+1.5)
}

// WebshareAudioCharJS builds the per-character typing snippet the Python
// injected for every captcha character (append + keydown/keypress/input/keyup
// events), with the same backslash/double-quote escaping.
func WebshareAudioCharJS(ch string) string {
	esc := strings.ReplaceAll(ch, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	return fmt.Sprintf(`const inp = document.getElementById('audio-response');
if (inp) {
    inp.value += "%s";
    inp.dispatchEvent(new KeyboardEvent('keydown', { key: "%s", bubbles: true }));
    inp.dispatchEvent(new KeyboardEvent('keypress', { key: "%s", bubbles: true }));
    inp.dispatchEvent(new Event('input', { bubbles: true }));
    inp.dispatchEvent(new KeyboardEvent('keyup', { key: "%s", bubbles: true }));
}`, esc, esc, esc, esc)
}

// ---------------------------------------------------------------------------
// Pure parsing helpers
// ---------------------------------------------------------------------------

var webshareProxyURLRegex = regexp.MustCompile(`^http://([^:]+):([^@]+)@([^:]+):(\d+)`)

var webshareIPv4Regex = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

var websharePortRegex = regexp.MustCompile(`^\d+$`)

// ParseWebshareProxyURL parses "http://user:pass@host:port", mirroring the
// `re.match(r'http://([^:]+):([^@]+)@([^:]+):(\d+)', p)` in sync_to_9router.
// ok is false when the string does not match.
func ParseWebshareProxyURL(s string) (user, pass, host string, port int, ok bool) {
	m := webshareProxyURLRegex.FindStringSubmatch(s)
	if m == nil {
		return "", "", "", 0, false
	}
	p, err := strconv.Atoi(m[4])
	if err != nil {
		return "", "", "", 0, false
	}
	return m[1], m[2], m[3], p, true
}

// FormatWebshareProxy builds the canonical "http://user:pass@ip:port" string.
func FormatWebshareProxy(user, pass, host string, port int) string {
	return fmt.Sprintf("http://%s:%s@%s:%d", user, pass, host, port)
}

// ParseWebshareDownloadLine parses one "ip:port:user:pass" line from the
// download-link API body, mirroring the harvester's split(':') == 4 branch.
func ParseWebshareDownloadLine(line string) (string, bool) {
	parts := strings.Split(line, ":")
	if len(parts) != 4 {
		return "", false
	}
	ip, port, user, pass := parts[0], parts[1], parts[2], parts[3]
	if ip == "" || port == "" || user == "" || pass == "" {
		return "", false
	}
	return FormatWebshareProxy(user, pass, ip, mustAtoiPort(port)), true
}

func mustAtoiPort(s string) int {
	p, _ := strconv.Atoi(s)
	return p
}

// ParseWebshareProxyTable converts raw DOM table rows (arrays of trimmed cell
// texts) into proxy URLs, mirroring the JS extraction branch of the smart
// polling loop: rows with >= 5 cells where cells[1..4] are
// ip, port, user, pass and ip/port match the numeric patterns.
func ParseWebshareProxyTable(rows [][]string) []string {
	var out []string
	for _, cells := range rows {
		if len(cells) < 5 {
			continue
		}
		ip, port, user, pass := cells[1], cells[2], cells[3], cells[4]
		if ip == "" || port == "" || user == "" || pass == "" {
			continue
		}
		if !webshareIPv4Regex.MatchString(ip) || !websharePortRegex.MatchString(port) {
			continue
		}
		out = append(out, FormatWebshareProxy(user, pass, ip, mustAtoiPort(port)))
	}
	return out
}

// WebshareJSBool converts a JS boolean eval result, like DrissionPage's bool.
func WebshareJSBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// WebshareJSString converts a JS string eval result.
func WebshareJSString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// WebshareJSStrings converts a JS string-array eval result.
func WebshareJSStrings(v any) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// WebshareJSRows converts a JS array-of-arrays eval result (the table rows).
func WebshareJSRows(v any) ([][]string, bool) {
	outer, ok := v.([]any)
	if !ok {
		return nil, false
	}
	rows := make([][]string, 0, len(outer))
	for _, r := range outer {
		inner, ok := r.([]any)
		if !ok {
			return nil, false
		}
		row := make([]string, 0, len(inner))
		for _, c := range inner {
			s, ok := c.(string)
			if !ok {
				return nil, false
			}
			row = append(row, s)
		}
		rows = append(rows, row)
	}
	return rows, true
}

// WebshareCleanDomain mirrors `custom_domain.strip().lstrip("@")`.
func WebshareCleanDomain(s string) string {
	return strings.TrimLeft(strings.TrimSpace(s), "@")
}

// ---------------------------------------------------------------------------
// Credential generation (mirrors hunt_single_auto's email/password builders)
// ---------------------------------------------------------------------------

const webshareEmailLocalChars = "abcdefghijklmnopqrstuvwxyz0123456789"
const websharePasswordMidChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
const websharePasswordSpecials = "!@#$%"

// WebshareRandomEmail builds "ws" + 10 lowercase alphanumerics + "@domain",
// mirroring the Python's email construction.
func WebshareRandomEmail(r *randv1.Rand, domain string) string {
	var b strings.Builder
	b.WriteString("ws")
	for i := 0; i < 10; i++ {
		b.WriteByte(webshareEmailLocalChars[r.Intn(len(webshareEmailLocalChars))])
	}
	b.WriteByte('@')
	b.WriteString(domain)
	return b.String()
}

// WebshareRandomPassword builds "Passw0rd" + one special + 6 alphanumerics +
// "@#", mirroring the Python's password construction.
func WebshareRandomPassword(r *randv1.Rand) string {
	var b strings.Builder
	b.WriteString("Passw0rd")
	b.WriteByte(websharePasswordSpecials[r.Intn(len(websharePasswordSpecials))])
	for i := 0; i < 6; i++ {
		b.WriteByte(websharePasswordMidChars[r.Intn(len(websharePasswordMidChars))])
	}
	b.WriteString("@#")
	return b.String()
}

// WebshareNewID generates a random 128-bit hex id for proxy pool entries
// (the Python used uuid.uuid4()).
func WebshareNewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand essentially never fails; fall back to math/rand so an
		// id is always produced.
		fb := randv1.New(randv1.NewSource(time.Now().UnixNano()))
		fb.Read(b[:])
	}
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// File + path helpers (mirrors append_to_proxies_txt, find_*_db/txt)
// ---------------------------------------------------------------------------

// AppendUniqueProxyLines appends proxy lines missing from the file, creating
// parent dirs as needed. It mirrors append_to_proxies_txt: returns the number
// of lines added and the total unique line count afterwards.
func AppendUniqueProxyLines(path string, newProxies []string) (added int, total int, err error) {
	existing := make(map[string]struct{})
	if data, rerr := os.ReadFile(path); rerr == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if t := strings.TrimSpace(line); t != "" {
				existing[t] = struct{}{}
			}
		}
	} else if !os.IsNotExist(rerr) {
		return 0, 0, fmt.Errorf("petaniproxy.AppendUniqueProxyLines: read: %w", rerr)
	}

	if dir := filepath.Dir(filepath.Clean(path)); dir != "" {
		if merr := os.MkdirAll(dir, 0o755); merr != nil {
			return 0, 0, fmt.Errorf("petaniproxy.AppendUniqueProxyLines: mkdir: %w", merr)
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, 0, fmt.Errorf("petaniproxy.AppendUniqueProxyLines: open: %w", err)
	}
	defer f.Close()

	for _, p := range newProxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := existing[p]; ok {
			continue
		}
		if _, werr := f.WriteString(p + "\n"); werr != nil {
			return added, len(existing), fmt.Errorf("petaniproxy.AppendUniqueProxyLines: write: %w", werr)
		}
		existing[p] = struct{}{}
		added++
	}
	return added, len(existing), nil
}

// FindWebshareDefaultDB mirrors find_default_db: checks ROUTER_DB_PATH,
// BANSOS_ROUTER_DB, NINEROUTER_DB, then a few repo-layout candidates.
// NOTE: the Python resolved candidates relative to the module file (__file__),
// which has no Go equivalent; here they are resolved relative to the working
// directory. Returns "" when nothing is found.
func FindWebshareDefaultDB() string {
	for _, env := range []string{"ROUTER_DB_PATH", "BANSOS_ROUTER_DB", "NINEROUTER_DB"} {
		if p := os.Getenv(env); p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	candidates := []string{
		filepath.Join("eLrouter", "data", "db", "data.sqlite"),
		filepath.Join("9router-mibp-version", "data", "db", "data.sqlite"),
		filepath.Join("9router", "data", "db", "data.sqlite"),
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	return ""
}

// FindWebshareProxiesTxt mirrors find_grok_proxies_txt: checks
// PROXIES_TXT_PATH then the output/proxies.txt candidate. Returns "" when
// nothing is found.
func FindWebshareProxiesTxt() string {
	if p := os.Getenv("PROXIES_TXT_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if abs, err := filepath.Abs(filepath.Join("output", "proxies.txt")); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	return ""
}

// webshareCapsolverConfigFiles mirrors the config file names probed by
// check_capsolver_balance (config/settings.json, config.json, local_config.json, .env).
var webshareCapsolverConfigFiles = []string{
	filepath.Join("config", "settings.json"),
	"config.json",
	"local_config.json",
	".env",
}

// LookupCapsolverKey mirrors the key discovery in check_capsolver_balance:
// explicit env CAPSOLVER_API_KEY first, then each baseDir probed for the
// config files above (JSON keys CAPSOLVER_API_KEY / capsolver_api_key, or a
// CAPSOLVER_API_KEY= line in .env). Returns "" when no key is configured.
func LookupCapsolverKey(baseDirs []string) string {
	if key := strings.TrimSpace(os.Getenv("CAPSOLVER_API_KEY")); key != "" {
		return key
	}
	for _, dir := range baseDirs {
		for _, fname := range webshareCapsolverConfigFiles {
			data, err := os.ReadFile(filepath.Join(dir, fname))
			if err != nil {
				continue
			}
			if strings.HasSuffix(fname, ".json") {
				var c map[string]any
				if jerr := json.Unmarshal(data, &c); jerr != nil {
					continue
				}
				for _, k := range []string{"CAPSOLVER_API_KEY", "capsolver_api_key"} {
					if s, ok := c[k].(string); ok && strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
			} else {
				for _, line := range strings.Split(string(data), "\n") {
					if strings.HasPrefix(line, "CAPSOLVER_API_KEY=") {
						v := strings.Trim(strings.SplitN(line, "=", 2)[1], "\"' \t\r")
						if v = strings.TrimSpace(v); v != "" {
							return v
						}
					}
				}
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Email domain resolution (mirrors get_webshare_email_domain)
// ---------------------------------------------------------------------------

// WebshareDomainOptions configures ResolveWebshareEmailDomain.
type WebshareDomainOptions struct {
	// CustomDomain wins when non-empty (explicit UI/modal argument).
	CustomDomain string
	// EnvDomain is the pre-resolved WEBSHARE_EMAIL_DOMAIN / WEBSHARE_DOMAIN value.
	EnvDomain string
	// ConfigDirs are searched for config/settings.json, config.json,
	// local_config.json (keys custom_email_domain / webshare_email_domain /
	// email_domain).
	ConfigDirs []string
	// Client is used for the live DuckMail / mail.tm lookups; nil builds a
	// 3.5s-timeout client.
	Client *http.Client
	// Pick selects the index for random.choice; nil uses math/rand.
	Pick func(n int) int
	// DuckMailURL / MailTmURL override the live domain APIs (tests).
	DuckMailURL string
	MailTmURL   string
}

// webshareDomainConfigFiles mirrors the JSON config paths probed for the
// email domain.
var webshareDomainConfigFiles = []string{
	filepath.Join("config", "settings.json"),
	"config.json",
	"local_config.json",
}

func lookupWebshareDomainInConfigs(dirs []string) string {
	for _, dir := range dirs {
		for _, fname := range webshareDomainConfigFiles {
			data, err := os.ReadFile(filepath.Join(dir, fname))
			if err != nil {
				continue
			}
			var c map[string]any
			if jerr := json.Unmarshal(data, &c); jerr != nil {
				continue
			}
			for _, k := range []string{"custom_email_domain", "webshare_email_domain", "email_domain"} {
				if s, ok := c[k].(string); ok {
					if d := WebshareCleanDomain(s); d != "" {
						return d
					}
				}
			}
		}
	}
	return ""
}

// fetchWebshareLiveDomains GETs a hydra-style domain list (DuckMail or
// mail.tm) and returns domains whose flag (isVerified / isActive, default
// true like Python's .get(key, True)) passes. Any failure yields nil.
func fetchWebshareLiveDomains(ctx context.Context, client *http.Client, url, flagKey string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Members []struct {
			Domain     string `json:"domain"`
			IsVerified *bool  `json:"isVerified"`
			IsActive   *bool  `json:"isActive"`
		} `json:"hydra:member"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	var out []string
	for _, m := range body.Members {
		if m.Domain == "" {
			continue
		}
		okFlag := true
		switch flagKey {
		case "isVerified":
			if m.IsVerified != nil {
				okFlag = *m.IsVerified
			}
		case "isActive":
			if m.IsActive != nil {
				okFlag = *m.IsActive
			}
		}
		if okFlag {
			out = append(out, m.Domain)
		}
	}
	return out
}

// ResolveWebshareEmailDomain mirrors get_webshare_email_domain's priority
// chain: custom arg > env var > config files > DuckMail live API > mail.tm
// live API > verified fallback pool.
func ResolveWebshareEmailDomain(ctx context.Context, opts WebshareDomainOptions) string {
	pick := opts.Pick
	if pick == nil {
		pick = randv1.Intn
	}
	if d := WebshareCleanDomain(opts.CustomDomain); d != "" {
		return d
	}
	if d := WebshareCleanDomain(opts.EnvDomain); d != "" {
		return d
	}
	if d := lookupWebshareDomainInConfigs(opts.ConfigDirs); d != "" {
		return d
	}
	client := opts.Client
	if client == nil {
		client = webshareHTTPClient(3500 * time.Millisecond)
	}
	duckURL := opts.DuckMailURL
	if duckURL == "" {
		duckURL = "https://api.duckmail.sbs/domains"
	}
	if domains := fetchWebshareLiveDomains(ctx, client, duckURL, "isVerified"); len(domains) > 0 {
		return domains[pick(len(domains))]
	}
	mailURL := opts.MailTmURL
	if mailURL == "" {
		mailURL = "https://api.mail.tm/domains"
	}
	if domains := fetchWebshareLiveDomains(ctx, client, mailURL, "isActive"); len(domains) > 0 {
		return domains[pick(len(domains))]
	}
	return WebshareFallbackEmailDomains[pick(len(WebshareFallbackEmailDomains))]
}

// ---------------------------------------------------------------------------
// CapSolver balance check (mirrors check_capsolver_balance)
// ---------------------------------------------------------------------------

// webshareCapsolverAPIBase is a var so tests can point it at an httptest server.
var webshareCapsolverAPIBase = "https://api.capsolver.com"

// WebshareCapsolverStatus mirrors the dict returned by check_capsolver_balance.
type WebshareCapsolverStatus struct {
	HasKey      bool
	Balance     float64
	Status      string // NOT_CONFIGURED | READY | EMPTY | INVALID | TIMEOUT
	CanHeadless bool
	Message     string
}

// CheckCapsolverBalance mirrors check_capsolver_balance: empty key ->
// NOT_CONFIGURED; POST getBalance -> READY (>= $0.003), EMPTY, INVALID, or
// TIMEOUT on network failure.
func CheckCapsolverBalance(ctx context.Context, client *http.Client, key string) WebshareCapsolverStatus {
	if strings.TrimSpace(key) == "" {
		return WebshareCapsolverStatus{
			HasKey:      false,
			Status:      "NOT_CONFIGURED",
			CanHeadless: false,
			Message:     "Key Kosong (Free Audio Solver Aktif)",
		}
	}
	if client == nil {
		client = webshareHTTPClient(3 * time.Second)
	}
	payload, _ := json.Marshal(map[string]string{"clientKey": key})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webshareCapsolverAPIBase+"/getBalance", bytes.NewReader(payload))
	if err != nil {
		return WebshareCapsolverStatus{HasKey: true, Status: "TIMEOUT", Message: "Key Ada (Network Timeout)"}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return WebshareCapsolverStatus{HasKey: true, Status: "TIMEOUT", Message: "Key Ada (Network Timeout)"}
	}
	defer resp.Body.Close()
	var data struct {
		ErrorID   int     `json:"errorId"`
		ErrorCode string  `json:"errorCode"`
		Balance   float64 `json:"balance"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return WebshareCapsolverStatus{HasKey: true, Status: "TIMEOUT", Message: "Key Ada (Network Timeout)"}
	}
	if data.ErrorID == 0 {
		if data.Balance >= WebshareCapsolverMinBalanceUSD {
			return WebshareCapsolverStatus{
				HasKey:      true,
				Balance:     data.Balance,
				Status:      "READY",
				CanHeadless: true,
				Message:     fmt.Sprintf("Saldo Aktif: $%.3f (Headless Ready)", data.Balance),
			}
		}
		return WebshareCapsolverStatus{
			HasKey:      true,
			Balance:     data.Balance,
			Status:      "EMPTY",
			CanHeadless: false,
			Message:     fmt.Sprintf("Saldo Habis: $%.3f (Headless Dimatikan)", data.Balance),
		}
	}
	return WebshareCapsolverStatus{
		HasKey:      true,
		Status:      "INVALID",
		CanHeadless: false,
		Message:     fmt.Sprintf("Key Tidak Valid (%s)", data.ErrorCode),
	}
}

// webshareHTTPClient builds a small purpose-timeout client honoring proxy env.
func webshareHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
	}
}

// ---------------------------------------------------------------------------
// Interfaced-out dependencies: browser, captcha STT, persistence, clock
// ---------------------------------------------------------------------------

// ErrWebshareElementNotFound is returned by WebshareBrowser.FindElement when
// no element matches the selector (DrissionPage's page.ele() returned None).
var ErrWebshareElementNotFound = errors.New("petaniproxy: webshare element not found")

// WebshareRect is an element's bounding box; X/Y is the top-left corner and
// W/H the size, mirroring DrissionPage's rect.location / rect.size.
type WebshareRect struct {
	X, Y, W, H float64
}

// WebshareBrowser abstracts the Chromium automation the hunter needs.
// Production wiring: implement with chromedp (add it to go.mod first).
// Method -> chromedp mapping:
//   - Navigate: chromedp.Navigate
//   - URL: read from the tab target info (target.GetTargetInfo)
//   - Eval: runtime.Evaluate on the main frame, awaitPromise, returnByValue
//   - EvalInFrame: runtime.Evaluate with the frame's execution context id
//   - Frames: page.GetFrameTree, collecting frame ids
//   - DispatchMouseMove: input.DispatchMouseEvent with type mouseMoved
//   - FindElement: translate the DrissionPage-style selector to CSS/XPath and
//     run dom.QuerySelector; return ErrWebshareElementNotFound when absent.
//     Selector translation cheatsheet:
//     "@name=email" -> `[name="email"]`, "@type=email" -> `[type="email"]`,
//     "tag:input@type=checkbox" -> `input[type="checkbox"]`,
//     ".PrivateSwitchBase-input" -> as-is,
//     "text:Sign Up With Email" -> XPath `//*[contains(text(),'Sign Up With Email')]`,
//     "tag:button@@text():Download" -> `//button[contains(.,'Download')]`,
//     "tag:p@@text():Proxy List" -> `//p[contains(.,'Proxy List')]`
//   - Close: chromedp.Cancel on the allocator context
type WebshareBrowser interface {
	// Navigate loads the URL in the current tab (page.get).
	Navigate(ctx context.Context, url string) error
	// URL returns the current tab URL (page.url).
	URL(ctx context.Context) (string, error)
	// Eval runs JS in the main frame and returns the JSON-ish result
	// (page.run_js).
	Eval(ctx context.Context, script string) (any, error)
	// EvalInFrame runs JS inside one frame (frame.run_js).
	EvalInFrame(ctx context.Context, frameID string, script string) (any, error)
	// Frames lists frame ids of the current tab (page.get_frames).
	Frames(ctx context.Context) ([]string, error)
	// DispatchMouseMove sends a CDP Input.dispatchMouseEvent mouseMoved
	// (used by the Bezier mouse simulation).
	DispatchMouseMove(ctx context.Context, x, y int) error
	// FindElement locates one element; ErrWebshareElementNotFound when absent.
	FindElement(ctx context.Context, selector string) (WebshareElement, error)
	// Close quits the browser (browser.quit).
	Close() error
}

// WebshareElement abstracts a located DOM element.
// Production wiring: implement over a chromedp node id.
//   - Click: input.DispatchMouseEvent mousePressed/mouseReleased, or
//     dom.Focus + input key Enter; the Python called element.click()
//   - Rect: dom.GetBoxModel -> content quad -> bounding box
//   - Clear: focus + select-all + delete (the Python's element.clear())
//   - TypeChar: input.InsertText per character (the Python's element.input(ch))
type WebshareElement interface {
	Click(ctx context.Context) error
	Rect(ctx context.Context) (WebshareRect, bool)
	Clear(ctx context.Context) error
	TypeChar(ctx context.Context, ch string) error
}

// WebshareBrowserFactory creates one browser per hunted account.
// Production wiring: build Chromium with the Python's options (auto port,
// headless flag + WebshareHeadlessUserAgent + --window-size=1920,1080, or
// --start-maximized; always WebshareDefaultChromiumArgs) via a chromedp
// ExecAllocator, then return the WebshareBrowser implementation.
type WebshareBrowserFactory interface {
	NewBrowser(ctx context.Context, headless bool) (WebshareBrowser, error)
}

// WebshareAudioTranscript is the STT result for one captcha audio clip.
type WebshareAudioTranscript struct {
	// Text is the transcribed digit/word sequence to type into the
	// audio-response input.
	Text string
	// DurationSec is the clip length; the hunter waits
	// WebshareAudioListenWait(DurationSec) to mimic listening.
	DurationSec float64
}

// WebshareCaptchaSolver converts captcha audio bytes to text.
// Production wiring: an implementation needs an STT API key, e.g. call the
// OpenAI Whisper API (OPENAI_API_KEY) or a local faster-whisper sidecar over
// HTTP. The Python used speech_recognition (Google web STT) on a pydub-made
// wav; the Go interface takes the raw mp3 bytes and the implementation owns
// any transcoding.
type WebshareCaptchaSolver interface {
	SolveAudio(ctx context.Context, audioMP3 []byte) (WebshareAudioTranscript, error)
}

// WebshareStubCaptchaSolver is the default solver: it always errors, so audio
// challenges are skipped until a real WebshareCaptchaSolver is wired.
type WebshareStubCaptchaSolver struct{}

// SolveAudio implements WebshareCaptchaSolver.
func (WebshareStubCaptchaSolver) SolveAudio(ctx context.Context, audioMP3 []byte) (WebshareAudioTranscript, error) {
	return WebshareAudioTranscript{}, errors.New("petaniproxy: no captcha solver configured; provide a WebshareCaptchaSolver (STT API key required)")
}

// WebshareProxyPoolEntry mirrors one proxyPools row written by sync_to_9router.
// Production sqlite wiring (modernc.org/sqlite, already in go.mod) implements
// WebshareProxyStore with:
//
//	CREATE TABLE IF NOT EXISTS proxyPools (id TEXT PRIMARY KEY, isActive INTEGER,
//	  testStatus TEXT, data TEXT, createdAt TEXT, updatedAt TEXT)
//
// and stores data as the JSON object below (name "Webshare Resi (host:port)",
// proxyUrl, noProxy "", type "http", strictProxy false, lastTestedAt /
// lastError null), createdAt/updatedAt as UTC RFC3339 with 'Z'.
type WebshareProxyPoolEntry struct {
	ID           string
	Name         string
	ProxyURL     string
	NoProxy      string
	Type         string // "http"
	StrictProxy  bool
	LastTestedAt *time.Time
	LastError    *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// WebshareProxyPoolEntryData is the JSON payload stored in proxyPools.data.
func (e WebshareProxyPoolEntry) WebshareProxyPoolEntryData() map[string]any {
	return map[string]any{
		"name":         e.Name,
		"proxyUrl":     e.ProxyURL,
		"noProxy":      e.NoProxy,
		"type":         e.Type,
		"strictProxy":  e.StrictProxy,
		"lastTestedAt": e.LastTestedAt,
		"lastError":    e.LastError,
	}
}

// WebshareProxyStore abstracts proxy persistence (the Python wrote sqlite3
// directly). Production wiring implements these two methods against the real
// proxyPools table; see WebshareProxyPoolEntry for the schema.
type WebshareProxyStore interface {
	// ExistingProxyURLs returns the set of already-stored proxyUrl values
	// (dedup source for SyncWebshareToStore).
	ExistingProxyURLs(ctx context.Context) (map[string]struct{}, error)
	// InsertProxy stores one harvested proxy.
	InsertProxy(ctx context.Context, entry WebshareProxyPoolEntry) error
}

// WebshareMemoryStore is an in-memory WebshareProxyStore for tests and for
// runs without a database. It does NOT persist across processes; wire a real
// sqlite WebshareProxyStore for production.
type WebshareMemoryStore struct {
	mu      sync.Mutex
	entries map[string]WebshareProxyPoolEntry
}

// NewWebshareMemoryStore creates an empty in-memory store.
func NewWebshareMemoryStore() *WebshareMemoryStore {
	return &WebshareMemoryStore{entries: make(map[string]WebshareProxyPoolEntry)}
}

// ExistingProxyURLs implements WebshareProxyStore.
func (m *WebshareMemoryStore) ExistingProxyURLs(ctx context.Context) (map[string]struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]struct{}, len(m.entries))
	for url := range m.entries {
		out[url] = struct{}{}
	}
	return out, nil
}

// InsertProxy implements WebshareProxyStore.
func (m *WebshareMemoryStore) InsertProxy(ctx context.Context, entry WebshareProxyPoolEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[entry.ProxyURL] = entry
	return nil
}

// All returns the stored entries (test helper).
func (m *WebshareMemoryStore) All() []WebshareProxyPoolEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]WebshareProxyPoolEntry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	return out
}

// WebshareSleeper abstracts time.Sleep so the hunt orchestration is unit
// testable without real delays.
type WebshareSleeper interface {
	Sleep(d time.Duration)
}

// WebshareRealSleeper is the production WebshareSleeper.
type WebshareRealSleeper struct{}

// Sleep implements WebshareSleeper.
func (WebshareRealSleeper) Sleep(d time.Duration) { time.Sleep(d) }

// ---------------------------------------------------------------------------
// Hunter: config, construction, human-behavior primitives
// ---------------------------------------------------------------------------

// WebshareHunterConfig configures the hunter. Zero values select Python-parity
// defaults (see NewWebshareHunter).
type WebshareHunterConfig struct {
	// Total accounts to hunt; <= 0 means 1 (Python default).
	Total int
	// Headless launches Chromium without a window.
	Headless bool
	// CustomDomain overrides the registration email domain.
	CustomDomain string
	// OutputDir holds webshare_residential.txt and live_elite.txt;
	// "" means "output" under the working directory.
	OutputDir string
	// ProxiesTxtPath is the grok-farm feed file; "" auto-detects via
	// FindWebshareProxiesTxt ("" after detection = skip).
	ProxiesTxtPath string
	// Store receives harvested proxies; nil skips DB sync (the Python
	// skipped when no DB file was found).
	Store WebshareProxyStore
	// BrowserFactory creates the Chromium session; nil means hunts fail
	// (a chromedp implementation must be wired for real runs).
	BrowserFactory WebshareBrowserFactory
	// CaptchaSolver solves audio captchas; nil uses WebshareStubCaptchaSolver.
	CaptchaSolver WebshareCaptchaSolver
	// CapsolverKey enables the paid reCAPTCHA path; "" falls back to
	// LookupCapsolverKey over ConfigDirs.
	CapsolverKey string
	// ConfigDirs are searched for capsolver keys and email-domain config;
	// empty means ["."].
	ConfigDirs []string
	// HTTPClient for downloads/API calls; nil uses the shared DefaultClient.
	HTTPClient *http.Client
	// Rand drives jitter and credential generation; nil seeds from time.
	Rand *randv1.Rand
	// Sleeper abstracts delays; nil uses WebshareRealSleeper.
	Sleeper WebshareSleeper
	// Timing overrides; <= 0 selects the WebshareDefault* constants.
	LoginTimeout   time.Duration
	HarvestTimeout time.Duration
	SolverInterval time.Duration
	CooldownRetry  time.Duration
	AccountGap     time.Duration
	FinalHold      time.Duration
	// ChromiumArgs passed to the browser factory; nil uses
	// WebshareDefaultChromiumArgs.
	ChromiumArgs []string
	// HeadlessUserAgent; "" uses WebshareHeadlessUserAgent.
	HeadlessUserAgent string
	// Verbose prints hunt progress.
	Verbose bool
}

// WebshareHunter hunts Webshare residential proxies via browser automation.
type WebshareHunter struct {
	cfg        WebshareHunterConfig
	rand       *randv1.Rand
	sleeper    WebshareSleeper
	httpClient *http.Client
	store      WebshareProxyStore
	solver     WebshareCaptchaSolver
	factory    WebshareBrowserFactory
}

// NewWebshareHunter builds a hunter, filling Python-parity defaults for zero
// config values.
func NewWebshareHunter(cfg WebshareHunterConfig) *WebshareHunter {
	r := cfg.Rand
	if r == nil {
		r = randv1.New(randv1.NewSource(time.Now().UnixNano()))
	}
	sleeper := cfg.Sleeper
	if sleeper == nil {
		sleeper = WebshareRealSleeper{}
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = DefaultClient
	}
	solver := cfg.CaptchaSolver
	if solver == nil {
		solver = WebshareStubCaptchaSolver{}
	}
	if cfg.LoginTimeout <= 0 {
		cfg.LoginTimeout = WebshareDefaultLoginTimeout
	}
	if cfg.HarvestTimeout <= 0 {
		cfg.HarvestTimeout = WebshareDefaultHarvestTimeout
	}
	if cfg.SolverInterval <= 0 {
		cfg.SolverInterval = WebshareDefaultSolverInterval
	}
	if cfg.CooldownRetry <= 0 {
		cfg.CooldownRetry = WebshareDefaultCooldownRetry
	}
	if cfg.AccountGap <= 0 {
		cfg.AccountGap = WebshareDefaultAccountGap
	}
	if cfg.FinalHold <= 0 {
		cfg.FinalHold = WebshareDefaultFinalHold
	}
	if cfg.ChromiumArgs == nil {
		cfg.ChromiumArgs = WebshareDefaultChromiumArgs
	}
	if cfg.HeadlessUserAgent == "" {
		cfg.HeadlessUserAgent = WebshareHeadlessUserAgent
	}
	return &WebshareHunter{
		cfg:        cfg,
		rand:       r,
		sleeper:    sleeper,
		httpClient: httpClient,
		store:      cfg.Store,
		solver:     solver,
		factory:    cfg.BrowserFactory,
	}
}

func (h *WebshareHunter) logf(format string, args ...any) {
	if h.cfg.Verbose {
		fmt.Printf("[webshare] "+format+"\n", args...)
	}
}

func webshareSleepJitter(s WebshareSleeper, r *randv1.Rand, minSec, maxSec float64) {
	if s == nil || r == nil {
		return
	}
	s.Sleep(time.Duration(WebshareUniformFloat(r, minSec, maxSec) * float64(time.Second)))
}

// SimulateHumanMouse moves the cursor from a random start point (100..400)
// to (targetX,targetY) along a quadratic Bezier with a jittered control
// point, dispatching one CDP mouseMoved per step plus the Python's trailing
// 0.1..0.25s pause. It mirrors simulate_human_mouse (default steps=25).
func SimulateHumanMouse(ctx context.Context, b WebshareBrowser, targetX, targetY float64, steps int, r *randv1.Rand, s WebshareSleeper) error {
	if steps < 1 {
		steps = 1
	}
	startX := float64(r.Intn(301) + 100)
	startY := float64(r.Intn(301) + 100)
	ctrlX := (startX+targetX)/2 + float64(r.Intn(161)-80)
	ctrlY := (startY+targetY)/2 + float64(r.Intn(161)-80)
	for _, p := range WebshareMousePath(startX, startY, ctrlX, ctrlY, targetX, targetY, steps) {
		if err := b.DispatchMouseMove(ctx, int(p.X), int(p.Y)); err != nil {
			return fmt.Errorf("petaniproxy.SimulateHumanMouse: %w", err)
		}
		webshareSleepJitter(s, r, 0.008, 0.022)
	}
	webshareSleepJitter(s, r, 0.1, 0.25)
	return nil
}

// HumanClickWebshareElement mirrors human_click_element: it moves the mouse
// along a Bezier to the element center plus a uniform(-4,4) offset, then
// clicks. A failed simulation still attempts the click (the Python's inner
// try/except fell through to element.click()).
func HumanClickWebshareElement(ctx context.Context, b WebshareBrowser, elem WebshareElement, r *randv1.Rand, s WebshareSleeper) error {
	if rect, ok := elem.Rect(ctx); ok {
		cx := rect.X + rect.W/2 + WebshareUniformFloat(r, -4, 4)
		cy := rect.Y + rect.H/2 + WebshareUniformFloat(r, -4, 4)
		_ = SimulateHumanMouse(ctx, b, cx, cy, 25, r, s) // best effort
	}
	return elem.Click(ctx)
}

// TypeTextHumanly types text one rune at a time with uniform(minDelaySec,
// maxDelaySec) pauses, mirroring the per-character email/password loops.
func TypeTextHumanly(ctx context.Context, elem WebshareElement, text string, r *randv1.Rand, s WebshareSleeper, minDelaySec, maxDelaySec float64) error {
	for _, ch := range text {
		if err := elem.TypeChar(ctx, string(ch)); err != nil {
			return fmt.Errorf("petaniproxy.TypeTextHumanly: %w", err)
		}
		webshareSleepJitter(s, r, minDelaySec, maxDelaySec)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Captcha solving
// ---------------------------------------------------------------------------

func (h *WebshareHunter) evalString(ctx context.Context, b WebshareBrowser, script string) (string, bool) {
	v, err := b.Eval(ctx, script)
	if err != nil {
		return "", false
	}
	return WebshareJSString(v)
}

func (h *WebshareHunter) frameBool(ctx context.Context, b WebshareBrowser, frameID, script string) bool {
	v, err := b.EvalInFrame(ctx, frameID, script)
	if err != nil {
		return false
	}
	ok, _ := WebshareJSBool(v)
	return ok
}

func (h *WebshareHunter) frameString(ctx context.Context, b WebshareBrowser, frameID, script string) string {
	v, err := b.EvalInFrame(ctx, frameID, script)
	if err != nil {
		return ""
	}
	s, _ := WebshareJSString(v)
	return s
}

// webshareMaxAudioBytes caps a captcha mp3 download (the Python had no cap).
const webshareMaxAudioBytes = 32 << 20

func (h *WebshareHunter) downloadBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy.WebshareHunter.downloadBytes: %w", err)
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy.WebshareHunter.downloadBytes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("petaniproxy.WebshareHunter.downloadBytes: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, webshareMaxAudioBytes))
	if err != nil {
		return nil, fmt.Errorf("petaniproxy.WebshareHunter.downloadBytes: %w", err)
	}
	return data, nil
}

// solveAudioChallenge mirrors try_solve_audio: it walks the tab frames,
// switches to the audio challenge, downloads the mp3, transcribes it via the
// WebshareCaptchaSolver, types the transcript character by character with
// human delays, and clicks verify. Returns true when the verify button was
// pressed. Like the Python's blanket try/except, unexpected failures yield
// (false, nil); only context cancellation returns an error.
func (h *WebshareHunter) solveAudioChallenge(ctx context.Context, b WebshareBrowser) (bool, error) {
	frames, err := b.Frames(ctx)
	if err != nil {
		return false, nil
	}
	for _, frameID := range frames {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		hasInput := h.frameBool(ctx, b, frameID, webshareAudioHasInputJS)
		if !hasInput {
			if h.frameBool(ctx, b, frameID, webshareAudioHeadphoneJS) {
				h.logf("headphone button clicked, waiting for audio")
				h.sleeper.Sleep(4 * time.Second)
				_, _ = b.EvalInFrame(ctx, frameID, webshareAudioPlayJS)
				h.sleeper.Sleep(2 * time.Second)
				hasInput = h.frameBool(ctx, b, frameID, webshareAudioHasInputJS)
			}
		}
		if !hasInput {
			continue
		}
		if limited := h.frameString(ctx, b, frameID, webshareAudioLimitJS); strings.Contains(limited, "automated queries") {
			h.logf("audio challenge rate-limited by Google")
			return false, nil
		}
		mp3URL := h.frameString(ctx, b, frameID, webshareAudioSourceJS)
		if mp3URL == "" {
			h.sleeper.Sleep(2 * time.Second)
			continue
		}
		h.logf("downloading captcha audio")
		audio, err := h.downloadBytes(ctx, mp3URL)
		if err != nil {
			h.logf("audio download failed: %v", err)
			return false, nil
		}
		transcript, err := h.solver.SolveAudio(ctx, audio)
		if err != nil {
			h.logf("captcha solver failed: %v", err)
			return false, nil
		}
		h.logf("transcript: %q (%.1fs)", transcript.Text, transcript.DurationSec)
		h.sleeper.Sleep(time.Duration(WebshareAudioListenWait(transcript.DurationSec) * float64(time.Second)))
		_, _ = b.EvalInFrame(ctx, frameID, webshareAudioFocusClearJS)
		h.sleeper.Sleep(500 * time.Millisecond)
		for _, ch := range transcript.Text {
			_, _ = b.EvalInFrame(ctx, frameID, WebshareAudioCharJS(string(ch)))
			webshareSleepJitter(h.sleeper, h.rand, 0.12, 0.28)
		}
		webshareSleepJitter(h.sleeper, h.rand, 1.5, 2.5)
		if h.frameBool(ctx, b, frameID, webshareAudioVerifyJS) {
			h.logf("captcha verify button pressed")
			h.sleeper.Sleep(6 * time.Second)
			return true, nil
		}
	}
	return false, nil
}

// capsolverCreateTask mirrors the createTask POST in try_solve_capsolver.
func (h *WebshareHunter) capsolverCreateTask(ctx context.Context, client *http.Client, key, sitekey, websiteURL string) string {
	payload, _ := json.Marshal(map[string]any{
		"clientKey": key,
		"task": map[string]string{
			"type":       "ReCaptchaV2TaskProxyLess",
			"websiteURL": websiteURL,
			"websiteKey": sitekey,
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webshareCapsolverAPIBase+"/createTask", bytes.NewReader(payload))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var data struct {
		TaskID string `json:"taskId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return ""
	}
	return data.TaskID
}

// capsolverTaskResult mirrors one getTaskResult poll: it returns the token
// when ready, or failed=true when the task failed.
func (h *WebshareHunter) capsolverTaskResult(ctx context.Context, client *http.Client, key, taskID string) (token string, failed bool) {
	payload, _ := json.Marshal(map[string]string{"clientKey": key, "taskId": taskID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webshareCapsolverAPIBase+"/getTaskResult", bytes.NewReader(payload))
	if err != nil {
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var data struct {
		Status   string `json:"status"`
		Solution struct {
			GRecaptchaResponse string `json:"gRecaptchaResponse"`
		} `json:"solution"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", false
	}
	switch data.Status {
	case "ready":
		return data.Solution.GRecaptchaResponse, false
	case "failed":
		return "", true
	default:
		return "", false
	}
}

// solveWithCapsolver mirrors try_solve_capsolver: extracts the sitekey,
// creates the task, polls up to WebshareCapsolverMaxPolls times, and injects
// the token into #g-recaptcha-response.
func (h *WebshareHunter) solveWithCapsolver(ctx context.Context, b WebshareBrowser, key string) bool {
	sitekey, ok := h.evalString(ctx, b, webshareSitekeyJS)
	if !ok || sitekey == "" {
		return false
	}
	h.logf("capsolver: sitekey %s", sitekey)
	client := webshareHTTPClient(10 * time.Second)
	websiteURL, _ := b.URL(ctx)
	taskID := h.capsolverCreateTask(ctx, client, key, sitekey, websiteURL)
	if taskID == "" {
		h.logf("capsolver: createTask failed")
		return false
	}
	for i := 0; i < WebshareCapsolverMaxPolls; i++ {
		h.sleeper.Sleep(WebshareCapsolverPollInterval)
		if err := ctx.Err(); err != nil {
			return false
		}
		token, failed := h.capsolverTaskResult(ctx, client, key, taskID)
		if failed {
			return false
		}
		if token != "" {
			h.logf("capsolver: token received")
			_, _ = b.Eval(ctx, fmt.Sprintf(
				`const el = document.getElementById('g-recaptcha-response'); if (el) el.value = %s;`,
				strconv.Quote(token)))
			return true
		}
	}
	return false
}

// capsolverKey resolves the key: explicit config first, then
// LookupCapsolverKey over the configured dirs.
func (h *WebshareHunter) capsolverKey() string {
	if k := strings.TrimSpace(h.cfg.CapsolverKey); k != "" {
		return k
	}
	dirs := h.cfg.ConfigDirs
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	return LookupCapsolverKey(dirs)
}

// trySolvers mirrors the monitor loop's solver dispatch: paid CapSolver when
// a key exists, otherwise the free audio solver.
func (h *WebshareHunter) trySolvers(ctx context.Context, b WebshareBrowser) {
	solved := false
	if key := h.capsolverKey(); key != "" {
		solved = h.solveWithCapsolver(ctx, b, key)
	}
	if !solved {
		_, _ = h.solveAudioChallenge(ctx, b)
	}
}

// ---------------------------------------------------------------------------
// Login monitor state machine (mirrors the 180s reCAPTCHA watch loop)
// ---------------------------------------------------------------------------

// WebshareMonitorDecision is one step of the login watch loop.
type WebshareMonitorDecision int

const (
	// WebshareMonitorKeepWaiting sleeps 3s and re-checks.
	WebshareMonitorKeepWaiting WebshareMonitorDecision = iota
	// WebshareMonitorSolve runs the captcha solvers now.
	WebshareMonitorSolve
	// WebshareMonitorCooldownRetry sleeps CooldownRetry then re-clicks signup
	// ("Too many attempts" branch).
	WebshareMonitorCooldownRetry
	// WebshareMonitorLoggedIn: left /register for dashboard.webshare.io.
	WebshareMonitorLoggedIn
	// WebshareMonitorTimedOut: LoginTimeout elapsed.
	WebshareMonitorTimedOut
)

// WebshareLoginMonitor is the pure state machine behind waitForLogin.
// Next is deterministic in (now, pageURL, tooManyAttempts), which makes the
// whole 180s polling loop unit-testable.
type WebshareLoginMonitor struct {
	start         time.Time
	deadline      time.Duration
	solveInterval time.Duration
	lastSolve     time.Time
	solvedOnce    bool
}

// NewWebshareLoginMonitor creates the monitor; start is usually time.Now().
func NewWebshareLoginMonitor(start time.Time, loginTimeout, solveInterval time.Duration) *WebshareLoginMonitor {
	return &WebshareLoginMonitor{start: start, deadline: loginTimeout, solveInterval: solveInterval}
}

// Next returns the next action. It mirrors the Python loop exactly: timeout
// check first, then the dashboard-URL login check, then the "Too many
// attempts" cooldown, then the >15s solver throttle.
func (m *WebshareLoginMonitor) Next(now time.Time, pageURL string, tooManyAttempts bool) WebshareMonitorDecision {
	if now.Sub(m.start) >= m.deadline {
		return WebshareMonitorTimedOut
	}
	if !strings.Contains(pageURL, "register") && strings.Contains(pageURL, "dashboard.webshare.io") {
		return WebshareMonitorLoggedIn
	}
	if tooManyAttempts {
		return WebshareMonitorCooldownRetry
	}
	if !m.solvedOnce || now.Sub(m.lastSolve) > m.solveInterval {
		m.solvedOnce = true
		m.lastSolve = now
		return WebshareMonitorSolve
	}
	return WebshareMonitorKeepWaiting
}

func (h *WebshareHunter) checkTooManyAttempts(ctx context.Context, b WebshareBrowser) bool {
	v, err := b.Eval(ctx, webshareTooManyAttemptsJS)
	if err != nil {
		return false
	}
	ok, _ := WebshareJSBool(v)
	return ok
}

func (h *WebshareHunter) findElement(ctx context.Context, b WebshareBrowser, selectors []string) (WebshareElement, bool) {
	for _, sel := range selectors {
		el, err := b.FindElement(ctx, sel)
		if err == nil && el != nil {
			return el, true
		}
	}
	return nil, false
}

func (h *WebshareHunter) clickSignupButton(ctx context.Context, b WebshareBrowser) {
	if el, ok := h.findElement(ctx, b, []string{"text:Sign Up With Email", "text:Sign Up"}); ok {
		_ = HumanClickWebshareElement(ctx, b, el, h.rand, h.sleeper)
	}
}

// waitForLogin mirrors the Python's 180s monitor loop via WebshareLoginMonitor.
func (h *WebshareHunter) waitForLogin(ctx context.Context, b WebshareBrowser) (bool, error) {
	mon := NewWebshareLoginMonitor(time.Now(), h.cfg.LoginTimeout, h.cfg.SolverInterval)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		pageURL, _ := b.URL(ctx)
		switch mon.Next(time.Now(), pageURL, h.checkTooManyAttempts(ctx, b)) {
		case WebshareMonitorLoggedIn:
			h.logf("logged in to dashboard")
			return true, nil
		case WebshareMonitorTimedOut:
			h.logf("login wait timed out")
			return false, nil
		case WebshareMonitorCooldownRetry:
			h.logf("too many attempts; cooling down")
			h.sleeper.Sleep(h.cfg.CooldownRetry)
			h.clickSignupButton(ctx, b)
		case WebshareMonitorSolve:
			h.trySolvers(ctx, b)
		case WebshareMonitorKeepWaiting:
		}
		h.sleeper.Sleep(3 * time.Second)
	}
}

// ---------------------------------------------------------------------------
// Harvest state machine + orchestration (mirrors the smart polling loop)
// ---------------------------------------------------------------------------

// WebshareHarvestDecision is one step of the dashboard smart-polling loop.
type WebshareHarvestDecision int

const (
	// WebshareHarvestWait sleeps 1.5s and re-polls.
	WebshareHarvestWait WebshareHarvestDecision = iota
	// WebshareHarvestFetchDownload downloads the proxy list from the
	// discovered download-link input value.
	WebshareHarvestFetchDownload
	// WebshareHarvestUseTable takes the proxies parsed from the DOM table.
	WebshareHarvestUseTable
	// WebshareHarvestClickDownload clicks the Download button (no link yet).
	WebshareHarvestClickDownload
	// WebshareHarvestClickNav navigates to the proxy list page.
	WebshareHarvestClickNav
)

// WebshareHarvestSignal is the observable page state for one poll iteration.
type WebshareHarvestSignal struct {
	PageURL           string
	DownloadURL       string // value of the proxy/list/download input, "" if absent
	TableProxies      []string
	HasDownloadButton bool
	HasNavButton      bool
}

// DecideWebshareHarvest mirrors the branch priority of the Python smart
// polling loop: download link (B) > table extraction (C) > Download button
// (D) > navigate to proxy list (E) > wait.
func DecideWebshareHarvest(s WebshareHarvestSignal) WebshareHarvestDecision {
	if s.DownloadURL != "" {
		return WebshareHarvestFetchDownload
	}
	if len(s.TableProxies) > 0 {
		return WebshareHarvestUseTable
	}
	if s.HasDownloadButton {
		return WebshareHarvestClickDownload
	}
	if !strings.Contains(s.PageURL, "proxy/list") {
		return WebshareHarvestClickNav
	}
	return WebshareHarvestWait
}

// downloadProxyList GETs the download-link URL and parses "ip:port:user:pass"
// lines, mirroring harvest branch B.
func (h *WebshareHunter) downloadProxyList(ctx context.Context, dlURL string) []string {
	data, err := h.downloadBytes(ctx, dlURL)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var proxies []string
	for _, line := range strings.Split(string(data), "\n") {
		if p, ok := ParseWebshareDownloadLine(strings.TrimSpace(line)); ok {
			proxies = append(proxies, p)
		}
	}
	return proxies
}

// harvestProxies mirrors the 60s smart polling loop in hunt_single_auto.
func (h *WebshareHunter) harvestProxies(ctx context.Context, b WebshareBrowser) ([]string, error) {
	deadline := time.Now().Add(h.cfg.HarvestTimeout)
	dlSelectors := []string{"text:Download", "tag:button@@text():Download"}
	navSelectors := []string{"text:Go To Proxy List", "text:View My Proxy List", "tag:p@@text():Proxy List"}
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, _ = b.Eval(ctx, webshareDismissModalsJS)
		pageURL, _ := b.URL(ctx)
		dlURL, _ := h.evalString(ctx, b, webshareDownloadLinkJS)
		var tableProxies []string
		if v, err := b.Eval(ctx, webshareTableRowsJS); err == nil {
			if rows, ok := WebshareJSRows(v); ok {
				tableProxies = ParseWebshareProxyTable(rows)
			}
		}
		dlEl, hasDlBtn := h.findElement(ctx, b, dlSelectors)
		navEl, hasNavBtn := h.findElement(ctx, b, navSelectors)
		switch DecideWebshareHarvest(WebshareHarvestSignal{
			PageURL:           pageURL,
			DownloadURL:       dlURL,
			TableProxies:      tableProxies,
			HasDownloadButton: hasDlBtn,
			HasNavButton:      hasNavBtn,
		}) {
		case WebshareHarvestFetchDownload:
			h.logf("download link found: %s", dlURL)
			if proxies := h.downloadProxyList(ctx, dlURL); len(proxies) > 0 {
				h.logf("harvested %d proxies via download link", len(proxies))
				return proxies, nil
			}
		case WebshareHarvestUseTable:
			h.logf("harvested %d proxies from table", len(tableProxies))
			return tableProxies, nil
		case WebshareHarvestClickDownload:
			h.logf("clicking Download button")
			_ = HumanClickWebshareElement(ctx, b, dlEl, h.rand, h.sleeper)
			h.sleeper.Sleep(1500 * time.Millisecond)
			continue
		case WebshareHarvestClickNav:
			if hasNavBtn {
				h.logf("navigating to proxy list")
				_ = HumanClickWebshareElement(ctx, b, navEl, h.rand, h.sleeper)
				h.sleeper.Sleep(2 * time.Second)
				continue
			}
		case WebshareHarvestWait:
		}
		h.sleeper.Sleep(1500 * time.Millisecond)
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Single-account hunt (mirrors hunt_single_auto)
// ---------------------------------------------------------------------------

func (h *WebshareHunter) resolveEmailDomain(ctx context.Context) string {
	envDomain := os.Getenv("WEBSHARE_EMAIL_DOMAIN")
	if envDomain == "" {
		envDomain = os.Getenv("WEBSHARE_DOMAIN")
	}
	dirs := h.cfg.ConfigDirs
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	return ResolveWebshareEmailDomain(ctx, WebshareDomainOptions{
		CustomDomain: h.cfg.CustomDomain,
		EnvDomain:    envDomain,
		ConfigDirs:   dirs,
		Client:       h.httpClient,
		Pick:         func(n int) int { return h.rand.Intn(n) },
	})
}

// fillRegistrationForm mirrors hunt step 1: email + password with typing
// simulation and the ToS checkbox. Failures are best-effort like the Python's
// per-section try/except.
func (h *WebshareHunter) fillRegistrationForm(ctx context.Context, b WebshareBrowser, email, password string) {
	if el, ok := h.findElement(ctx, b, []string{"@name=email", "@type=email"}); ok {
		_ = HumanClickWebshareElement(ctx, b, el, h.rand, h.sleeper)
		_ = el.Clear(ctx)
		_ = TypeTextHumanly(ctx, el, email, h.rand, h.sleeper, 0.02, 0.07)
		h.sleeper.Sleep(400 * time.Millisecond)
	}
	if el, ok := h.findElement(ctx, b, []string{"@name=password", "@type=password"}); ok {
		_ = HumanClickWebshareElement(ctx, b, el, h.rand, h.sleeper)
		_ = el.Clear(ctx)
		_ = TypeTextHumanly(ctx, el, password, h.rand, h.sleeper, 0.02, 0.07)
		h.sleeper.Sleep(400 * time.Millisecond)
	}
	if el, ok := h.findElement(ctx, b, []string{"tag:input@type=checkbox", ".PrivateSwitchBase-input"}); ok {
		_ = HumanClickWebshareElement(ctx, b, el, h.rand, h.sleeper)
	} else {
		_, _ = b.Eval(ctx, webshareCheckboxJS)
	}
	h.logf("registration form filled")
}

// HuntSingle registers one Webshare account and harvests its residential
// proxies, mirroring hunt_single_auto. It returns the harvested proxy URLs
// (empty when login timed out or nothing was harvested). Only browser
// creation, navigation, and context cancellation return errors; page-level
// failures are best-effort like the Python's per-section try/except blocks.
func (h *WebshareHunter) HuntSingle(ctx context.Context, index, total int) ([]string, error) {
	if h.factory == nil {
		return nil, errors.New("petaniproxy.WebshareHunter.HuntSingle: no BrowserFactory configured; wire a chromedp implementation")
	}
	domain := h.resolveEmailDomain(ctx)
	email := WebshareRandomEmail(h.rand, domain)
	password := WebshareRandomPassword(h.rand)
	h.logf("account [%d/%d] email=%s", index, total, email)

	b, err := h.factory.NewBrowser(ctx, h.cfg.Headless)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy.WebshareHunter.HuntSingle: new browser: %w", err)
	}
	defer func() { _ = b.Close() }()

	if err := b.Navigate(ctx, WebshareRegisterURL); err != nil {
		return nil, fmt.Errorf("petaniproxy.WebshareHunter.HuntSingle: navigate: %w", err)
	}
	h.sleeper.Sleep(3 * time.Second)

	h.fillRegistrationForm(ctx, b, email, password)
	h.sleeper.Sleep(1 * time.Second)
	h.clickSignupButton(ctx, b)

	loggedIn, err := h.waitForLogin(ctx, b)
	if err != nil {
		return nil, err
	}
	if !loggedIn {
		h.logf("account [%d/%d] login timed out", index, total)
		return nil, nil
	}

	proxies, err := h.harvestProxies(ctx, b)
	if err != nil {
		return nil, err
	}
	if len(proxies) == 0 {
		h.logf("no proxies harvested; holding browser")
		h.sleeper.Sleep(h.cfg.FinalHold)
	}
	return proxies, nil
}

// ---------------------------------------------------------------------------
// Persistence sync (mirrors sync_to_9router)
// ---------------------------------------------------------------------------

// SyncWebshareToStore mirrors sync_to_9router: it inserts harvested proxies
// missing from the store, skipping malformed URLs. Returns the inserted count.
func SyncWebshareToStore(ctx context.Context, store WebshareProxyStore, proxies []string) (int, error) {
	existing, err := store.ExistingProxyURLs(ctx)
	if err != nil {
		return 0, fmt.Errorf("petaniproxy.SyncWebshareToStore: %w", err)
	}
	added := 0
	for _, p := range proxies {
		if _, ok := existing[p]; ok {
			continue
		}
		// user/pass stay embedded in the proxy URL itself; only host:port are
		// needed for the entry name, like the Python's unpacking.
		_, _, host, port, ok := ParseWebshareProxyURL(p)
		if !ok {
			continue
		}
		now := time.Now().UTC()
		entry := WebshareProxyPoolEntry{
			ID:          WebshareNewID(),
			Name:        fmt.Sprintf("Webshare Resi (%s:%d)", host, port),
			ProxyURL:    p,
			NoProxy:     "",
			Type:        "http",
			StrictProxy: false,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := store.InsertProxy(ctx, entry); err != nil {
			return added, fmt.Errorf("petaniproxy.SyncWebshareToStore: %w", err)
		}
		existing[p] = struct{}{}
		added++
	}
	return added, nil
}

// ---------------------------------------------------------------------------
// Multi-account run (mirrors run_webshare_hunter)
// ---------------------------------------------------------------------------

// RunWebshareHunter hunts cfg.Total accounts sequentially with the Python's
// 3s inter-account gap, appends harvests to output/webshare_residential.txt
// and output/live_elite.txt, feeds the grok-farm proxies.txt when detected,
// and syncs to the Store when configured. It mirrors run_webshare_hunter and
// returns every gathered proxy URL.
func RunWebshareHunter(ctx context.Context, cfg WebshareHunterConfig) ([]string, error) {
	h := NewWebshareHunter(cfg)
	total := cfg.Total
	if total <= 0 {
		total = 1
	}
	outDir := cfg.OutputDir
	if outDir == "" {
		outDir = "output"
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("petaniproxy.RunWebshareHunter: %w", err)
	}
	grokTxt := cfg.ProxiesTxtPath
	if grokTxt == "" {
		grokTxt = FindWebshareProxiesTxt()
	}
	webshareTxt := filepath.Join(outDir, "webshare_residential.txt")
	eliteTxt := filepath.Join(outDir, "live_elite.txt")

	var all []string
	for i := 1; i <= total; i++ {
		if err := ctx.Err(); err != nil {
			return all, err
		}
		proxies, err := h.HuntSingle(ctx, i, total)
		if err != nil {
			return all, fmt.Errorf("petaniproxy.RunWebshareHunter: account %d: %w", i, err)
		}
		if len(proxies) > 0 {
			h.logf("account [%d/%d] harvested %d proxies", i, total, len(proxies))
			if _, _, err := AppendUniqueProxyLines(webshareTxt, proxies); err != nil {
				return all, err
			}
			if _, _, err := AppendUniqueProxyLines(eliteTxt, proxies); err != nil {
				return all, err
			}
			if grokTxt != "" {
				if _, _, err := AppendUniqueProxyLines(grokTxt, proxies); err != nil {
					return all, err
				}
			}
			if h.store != nil {
				if _, err := SyncWebshareToStore(ctx, h.store, proxies); err != nil {
					return all, err
				}
			}
			all = append(all, proxies...)
		} else {
			h.logf("account [%d/%d] harvested nothing", i, total)
		}
		if i < total {
			h.sleeper.Sleep(h.cfg.AccountGap)
		}
	}
	return all, nil
}
