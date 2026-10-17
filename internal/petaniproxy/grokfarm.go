// Package petaniproxy — Grok xAI account farm engine.
//
// This file is a Go port of petani-proxy's core/grok_farm.py: an automated
// Grok xAI account harvester. The Python original drives a real Chromium via
// DrissionPage (CDP) and reads OTP codes over IMAP (imaplib). Neither of those
// dependencies exists in the Go standard library, so this port is structured
// around two interface seams:
//
//   - Browser automation: implement GrokBrowserDriver (Navigate, RunJS,
//     Cookies, Close) on top of chromedp or a raw CDP WebSocket client, then
//     pass a GrokBrowserFactory in GrokRegisterConfig.NewBrowser. The JS
//     payloads are ported verbatim from the Python original and are embedded
//     as constants (GrokClickEmailSignupJS, GrokFillEmailJS, GrokFillOTPJS,
//     GrokFillOTPSubmitJS, GrokFillProfileJS).
//   - Email/IMAP: Go's stdlib has no IMAP client. Implement the EmailClient
//     interface (Search, Fetch, MarkRead, Close) on top of a third-party IMAP
//     library (e.g. github.com/emersion/go-imap) and pass it via
//     GmailMailboxService.NewEmailClient / GrokRegisterConfig.NewEmailClient.
//     FakeEmailClient is an in-memory implementation for tests and local dev.
//
// The DuckMail/Mail.tm disposable-mailbox path is plain HTTPS and is fully
// implemented with net/http (no extra dependency needed).
//
// Everything else — OTP extraction regexes, Gmail +tag aliasing, retry and
// polling loops, the signup state machine, cookie extraction, and the
// output file formats — is ported faithfully from grok_farm.py and is
// covered by table-driven unit tests in grokfarm_test.go.
//
// A minimal main wiring looks like:
//
//	func main() {
//	    cfg := petaniproxy.GrokFarmConfig{
//	        Total: 3, Headless: true, MailProvider: "gmail",
//	        GmailUser: os.Getenv("GROK_GMAIL_USER"),
//	        GmailAppPassword: os.Getenv("GROK_GMAIL_APP_PASSWORD"),
//	        NewBrowser: myChromedpFactory, // func(ctx, opts) (petaniproxy.GrokBrowserDriver, error)
//	    }
//	    // wire cfg.NewEmailClient with a go-imap adapter before running
//	    accounts, err := petaniproxy.RunGrokFarm(context.Background(), cfg)
//	    ...
//	}
package petaniproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GrokSignupURL is the xAI registration landing page the farm drives.
const GrokSignupURL = "https://accounts.x.ai/sign-up?redirect=grok-com"

// GrokChromeUserAgent mirrors the UA the Python farm sets on Chromium.
const GrokChromeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

// GrokMaxStateLogs caps the in-memory log ring, matching grok_farm.py.
const GrokMaxStateLogs = 60

// Log levels understood by GrokFarmState.Log.
const (
	GrokLogInfo    = "info"
	GrokLogWarning = "warning"
	GrokLogError   = "error"
	GrokLogSuccess = "success"
	GrokLogDebug   = "debug"
)

// Farm status values.
const (
	GrokStatusIdle      = "idle"
	GrokStatusRunning   = "running"
	GrokStatusCompleted = "completed"
	GrokStatusError     = "error"
)

// Signup step names, mirrored from the Python grok_farm_state["current_step"].
const (
	GrokStepIdle           = "idle"
	GrokStepCreatingEmail  = "creating_email"
	GrokStepOpeningBrowser = "opening_browser"
	GrokStepEnteringEmail  = "entering_email"
	GrokStepWaitingOTP     = "waiting_otp"
	GrokStepVerifyingOTP   = "verifying_otp"
	GrokStepSettingProfile = "setting_profile"
	GrokStepFinalizing     = "finalizing"
	GrokStepCompleted      = "completed"
)

// errGrokNoIMAPClient is returned when a Gmail flow is attempted without an
// EmailClient factory wired in (production needs a go-imap adapter).
var errGrokNoIMAPClient = errors.New("grokfarm: no IMAP EmailClient configured; set GmailMailboxService.NewEmailClient with a go-imap backed implementation")

// GrokDefaultHTTPClient is the shared client for DuckMail/Mail.tm API calls
// when the caller does not supply one.
var GrokDefaultHTTPClient = &http.Client{Timeout: 15 * time.Second}

// ---------------------------------------------------------------------------
// Farm state (dashboard/terminal sync)
// ---------------------------------------------------------------------------

// GrokHarvestedSummary is the dashboard-facing summary of one harvested account.
type GrokHarvestedSummary struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	CreatedAt string `json:"created_at"`
	HasSSO    bool   `json:"has_sso"`
}

// GrokFarmSnapshot is a consistent, mutex-free copy of GrokFarmState.
type GrokFarmSnapshot struct {
	Status            string                 `json:"status"`
	Progress          int                    `json:"progress"`
	CurrentAccount    int                    `json:"current_account"`
	TotalAccounts     int                    `json:"total_accounts"`
	HarvestedCount    int                    `json:"harvested_count"`
	HarvestedAccounts []GrokHarvestedSummary `json:"harvested_accounts"`
	Logs              []string               `json:"logs"`
	CurrentStep       string                 `json:"current_step"`
	LastError         string                 `json:"last_error,omitempty"`
}

// GrokFarmState is the thread-safe equivalent of grok_farm.py's
// grok_farm_state dict + grok_lock. It feeds both terminal logs and the web
// dashboard. The zero value is usable via NewGrokFarmState.
type GrokFarmState struct {
	mu                sync.Mutex
	status            string
	progress          int
	currentAccount    int
	totalAccounts     int
	harvestedCount    int
	harvestedAccounts []GrokHarvestedSummary
	logs              []string
	currentStep       string
	lastError         string
}

// NewGrokFarmState returns an idle farm state.
func NewGrokFarmState() *GrokFarmState {
	return &GrokFarmState{status: GrokStatusIdle, currentStep: GrokStepIdle}
}

// Log prints a timestamped line to stdout (like grok_log's colorama output,
// without ANSI colors since this port is stdlib-only) and appends it to the
// state log ring (capped at GrokMaxStateLogs). A non-empty step also updates
// the current step.
func (s *GrokFarmState) Log(message, level, step string) {
	now := time.Now().Format("15:04:05")
	clean := fmt.Sprintf("[%s] %s", now, message)

	marker := "[*]"
	switch level {
	case GrokLogError:
		marker = "[!]"
	case GrokLogSuccess:
		marker = "[+]"
	case GrokLogDebug:
		marker = "[~]"
	}
	fmt.Printf("%s %s\n", marker, clean)

	s.mu.Lock()
	defer s.mu.Unlock()
	if step != "" {
		s.currentStep = step
	}
	s.logs = append(s.logs, clean)
	if len(s.logs) > GrokMaxStateLogs {
		s.logs = s.logs[len(s.logs)-GrokMaxStateLogs:]
	}
}

// Snapshot returns a consistent copy of the state for dashboard rendering.
func (s *GrokFarmState) Snapshot() GrokFarmSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	logs := make([]string, len(s.logs))
	copy(logs, s.logs)
	accs := make([]GrokHarvestedSummary, len(s.harvestedAccounts))
	copy(accs, s.harvestedAccounts)
	return GrokFarmSnapshot{
		Status:            s.status,
		Progress:          s.progress,
		CurrentAccount:    s.currentAccount,
		TotalAccounts:     s.totalAccounts,
		HarvestedCount:    s.harvestedCount,
		HarvestedAccounts: accs,
		Logs:              logs,
		CurrentStep:       s.currentStep,
		LastError:         s.lastError,
	}
}

func (s *GrokFarmState) begin(total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = GrokStatusRunning
	s.progress = 5
	s.currentAccount = 0
	s.totalAccounts = total
	s.harvestedCount = 0
	s.harvestedAccounts = nil
	s.logs = nil
	s.lastError = ""
	s.currentStep = GrokStepIdle
}

func (s *GrokFarmState) setCurrent(i, progress int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentAccount = i
	s.progress = progress
}

func (s *GrokFarmState) addHarvested(acc GrokAccount) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.harvestedAccounts = append(s.harvestedAccounts, GrokHarvestedSummary{
		Email:     acc.Email,
		Password:  acc.Password,
		CreatedAt: acc.CreatedAt,
		HasSSO:    acc.SSOToken != "",
	})
	s.harvestedCount = len(s.harvestedAccounts)
}

func (s *GrokFarmState) finish(harvested, total int) {
	s.mu.Lock()
	s.status = GrokStatusCompleted
	s.progress = 100
	s.currentStep = GrokStepCompleted
	s.mu.Unlock()
	s.Log(fmt.Sprintf("Proses ternak selesai! Berhasil memanen %d/%d akun Grok xAI.", harvested, total), GrokLogSuccess, "")
}

// ---------------------------------------------------------------------------
// Harvested account
// ---------------------------------------------------------------------------

// GrokCookie is a session cookie extracted from the signup browser session.
type GrokCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	Path     string `json:"path,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"httpOnly,omitempty"`
	Expires  int64  `json:"expires,omitempty"`
}

// GrokAccount is one harvested Grok xAI account.
type GrokAccount struct {
	Email     string       `json:"email"`
	Password  string       `json:"password"`
	SSOToken  string       `json:"sso_token"`
	Cookies   []GrokCookie `json:"cookies"`
	CreatedAt string       `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Residential proxy pool discovery
// ---------------------------------------------------------------------------

// FindResidentialProxies scans the known proxy-list candidates under baseDir
// (output/webshare_residential.txt, output/live_elite.txt,
// ../grok-register/proxies.txt, with PROXIES_TXT_PATH env checked first) and
// returns the lines of the first file that yields any proxies. Blank lines
// and "#" comments are skipped, mirroring find_residential_proxies().
func FindResidentialProxies(baseDir string) []string {
	candidates := []string{
		filepath.Join(baseDir, "output", "webshare_residential.txt"),
		filepath.Join(baseDir, "output", "live_elite.txt"),
		filepath.Join(baseDir, "..", "grok-register", "proxies.txt"),
	}
	if env := strings.TrimSpace(os.Getenv("PROXIES_TXT_PATH")); env != "" {
		candidates = append([]string{env}, candidates...)
	}
	for _, c := range candidates {
		p, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var proxies []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			proxies = append(proxies, line)
		}
		if len(proxies) > 0 {
			return proxies
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Gmail credentials
// ---------------------------------------------------------------------------

// LoadGmailCredentials returns the Gmail IMAP credentials from the
// GROK_GMAIL_USER and GROK_GMAIL_APP_PASSWORD environment variables (app
// password spaces are stripped, like the Python loader).
//
// NOTE: the Python original falls back to a hardcoded user/app-password pair
// when no config file is found. That fallback is deliberately NOT ported:
// baking an app password into source is a credential leak. Set the env vars
// (or wire ParseGmailCredentialsTOML/ParseGmailCredentialsJSON yourself).
func LoadGmailCredentials() (user, appPassword string) {
	user = strings.TrimSpace(os.Getenv("GROK_GMAIL_USER"))
	appPassword = strings.TrimSpace(strings.ReplaceAll(os.Getenv("GROK_GMAIL_APP_PASSWORD"), " ", ""))
	return user, appPassword
}

var (
	grokTomlUserRe = regexp.MustCompile(`gmail_user\s*=\s*["']([^"']+)["']`)
	grokTomlPassRe = regexp.MustCompile(`gmail_app_password\s*=\s*["']([^"']+)["']`)
)

// ParseGmailCredentialsTOML extracts gmail_user / gmail_app_password from a
// TOML-ish config text, mirroring load_gmail_credentials()'s regex scan of
// qoder-creator's config.toml.
func ParseGmailCredentialsTOML(text string) (user, appPassword string, ok bool) {
	um := grokTomlUserRe.FindStringSubmatch(text)
	pm := grokTomlPassRe.FindStringSubmatch(text)
	if len(um) < 2 || len(pm) < 2 {
		return "", "", false
	}
	return strings.TrimSpace(um[1]), strings.TrimSpace(strings.ReplaceAll(pm[1], " ", "")), true
}

// ParseGmailCredentialsJSON extracts gmail_user/GMAIL_USER and
// gmail_app_password/GMAIL_APP_PASSWORD from a JSON settings blob, mirroring
// the settings.json branch of load_gmail_credentials().
func ParseGmailCredentialsJSON(data []byte) (user, appPassword string, ok bool) {
	var d map[string]any
	if err := json.Unmarshal(data, &d); err != nil {
		return "", "", false
	}
	u := grokFirstNonEmptyString(d, "gmail_user", "GMAIL_USER")
	p := grokFirstNonEmptyString(d, "gmail_app_password", "GMAIL_APP_PASSWORD")
	if u == "" || p == "" {
		return "", "", false
	}
	return strings.TrimSpace(u), strings.TrimSpace(strings.ReplaceAll(p, " ", "")), true
}

func grokFirstNonEmptyString(d map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := d[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// EmailClient interface — IMAP seam
// ---------------------------------------------------------------------------

// GrokEmailMessage is a parsed inbound email: the headers the OTP matcher
// needs plus the decoded text body.
type GrokEmailMessage struct {
	To          string
	DeliveredTo string
	Subject     string
	From        string
	Body        string
}

// EmailClient is the IMAP seam for OTP polling. Go's stdlib has no IMAP
// client, so production must implement this on top of a third-party IMAP
// library (e.g. github.com/emersion/go-imap): Search lists message IDs
// (criteria like "ALL"), Fetch returns the parsed message, MarkRead flags a
// message as seen, Close logs out.
type EmailClient interface {
	Search(ctx context.Context, criteria string) ([]string, error)
	Fetch(ctx context.Context, id string) (*GrokEmailMessage, error)
	MarkRead(ctx context.Context, id string) error
	Close() error
}

// FakeEmailClient is an in-memory EmailClient for tests and local dev. Use
// AddMessage to seed the inbox the fake will serve.
type FakeEmailClient struct {
	mu         sync.Mutex
	ids        []string
	msgs       map[string]*GrokEmailMessage
	markedRead []string
	closed     bool
	// SearchErr / FetchErr, when non-nil, are returned by the matching call.
	SearchErr error
	FetchErr  error
}

// NewFakeEmailClient returns an empty in-memory email client.
func NewFakeEmailClient() *FakeEmailClient {
	return &FakeEmailClient{msgs: make(map[string]*GrokEmailMessage)}
}

// AddMessage seeds one message under the given ID.
func (f *FakeEmailClient) AddMessage(id string, m GrokEmailMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.msgs[id]; !exists {
		f.ids = append(f.ids, id)
	}
	cp := m
	f.msgs[id] = &cp
}

func (f *FakeEmailClient) Search(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SearchErr != nil {
		return nil, f.SearchErr
	}
	out := make([]string, len(f.ids))
	copy(out, f.ids)
	return out, nil
}

func (f *FakeEmailClient) Fetch(_ context.Context, id string) (*GrokEmailMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FetchErr != nil {
		return nil, f.FetchErr
	}
	m, ok := f.msgs[id]
	if !ok {
		return nil, fmt.Errorf("grokfarm: fake message %q not found", id)
	}
	cp := *m
	return &cp, nil
}

func (f *FakeEmailClient) MarkRead(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markedRead = append(f.markedRead, id)
	return nil
}

func (f *FakeEmailClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// MarkedReadIDs returns the IDs flagged via MarkRead, in call order.
func (f *FakeEmailClient) MarkedReadIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.markedRead))
	copy(out, f.markedRead)
	return out
}

// Closed reports whether Close was called.
func (f *FakeEmailClient) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

var _ EmailClient = (*FakeEmailClient)(nil)

// ---------------------------------------------------------------------------
// Email parsing + OTP extraction (pure logic)
// ---------------------------------------------------------------------------

// ParseGrokEmailMessage parses a raw RFC822 message (the equivalent of
// email.message_from_bytes in poll_verification_code), extracting the
// To/Delivered-To/Subject/From headers and concatenating the decoded
// text/plain and text/html parts (multipart-aware, base64 and
// quoted-printable decoded, undecodable bytes replaced like errors="replace").
func ParseGrokEmailMessage(raw []byte) (GrokEmailMessage, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return GrokEmailMessage{}, fmt.Errorf("grokfarm: parse email: %w", err)
	}
	hdr := m.Header
	out := GrokEmailMessage{
		To:          hdr.Get("To"),
		DeliveredTo: hdr.Get("Delivered-To"),
		Subject:     hdr.Get("Subject"),
		From:        hdr.Get("From"),
	}
	var body strings.Builder
	mediaType, params, perr := mime.ParseMediaType(hdr.Get("Content-Type"))
	if perr == nil && strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(m.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			ct := part.Header.Get("Content-Type")
			if strings.Contains(ct, "text/plain") || strings.Contains(ct, "text/html") {
				b, rerr := io.ReadAll(grokDecodePartReader(part, part.Header.Get("Content-Transfer-Encoding")))
				if rerr == nil {
					body.Write(b)
					body.WriteByte(' ')
				}
			}
		}
	} else {
		b, rerr := io.ReadAll(grokDecodePartReader(m.Body, hdr.Get("Content-Transfer-Encoding")))
		if rerr == nil {
			body.Write(b)
		}
	}
	out.Body = body.String()
	return out, nil
}

func grokDecodePartReader(r io.Reader, encoding string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	default:
		return r
	}
}

var (
	// grokOTPDashedRe matches the xAI "XXX-XXX" alphanumeric code format.
	grokOTPDashedRe = regexp.MustCompile(`\b([A-Za-z0-9]{3}-[A-Za-z0-9]{3})\b`)
	// grokOTP6Re matches a plain 6-digit code.
	grokOTP6Re = regexp.MustCompile(`\b(\d{6})\b`)
	// grokDuckCodeRe matches the DuckMail poll patterns: 6 digits or 3-3 digits.
	grokDuckCodeRe = regexp.MustCompile(`\b(\d{6})\b|\b(\d{3}-\d{3})\b`)
)

// ExtractGrokOTP finds the xAI verification code in text. The dashed
// alphanumeric form (XXX-XXX, dashes stripped) wins over a plain 6-digit
// code, exactly like poll_verification_code's two-step regex search.
func ExtractGrokOTP(text string) string {
	if m := grokOTPDashedRe.FindStringSubmatch(text); len(m) > 1 {
		return strings.ReplaceAll(m[1], "-", "")
	}
	if m := grokOTP6Re.FindStringSubmatch(text); len(m) > 1 {
		return m[1]
	}
	return ""
}

// ExtractDuckMailCode finds the verification code in a DuckMail/Mail.tm
// message body, mirroring DuckMailService.poll_verification_code: the first
// regex hit wins, the first non-empty capture group is used, dashes stripped.
func ExtractDuckMailCode(text string) string {
	for _, m := range grokDuckCodeRe.FindAllStringSubmatch(text, -1) {
		for _, g := range m[1:] {
			if g != "" {
				return strings.ReplaceAll(g, "-", "")
			}
		}
	}
	return ""
}

// GrokEmailMatchesOTP reports whether msg is the OTP email for the account
// identified by targetAddr (lowercased alias) or tag, mirroring the is_target
// + sender/subject checks in GmailImapService.poll_verification_code.
func GrokEmailMatchesOTP(msg GrokEmailMessage, targetAddr, tag string) bool {
	target := strings.ToLower(strings.TrimSpace(targetAddr))
	if target == "" {
		return false
	}
	to := strings.ToLower(msg.To)
	delivered := strings.ToLower(msg.DeliveredTo)
	isTarget := strings.Contains(to, target) || strings.Contains(delivered, target)
	if !isTarget && tag != "" {
		t := strings.ToLower(tag)
		isTarget = strings.Contains(to, t) || strings.Contains(delivered, t)
	}
	if !isTarget {
		return false
	}
	from := strings.ToLower(msg.From)
	subject := strings.ToLower(msg.Subject)
	return strings.Contains(from, "xai") ||
		strings.Contains(from, "grok") ||
		strings.Contains(from, "x.ai") ||
		strings.Contains(subject, "code") ||
		strings.Contains(subject, "verif")
}

// ---------------------------------------------------------------------------
// Random identity helpers (pure logic, seedable)
// ---------------------------------------------------------------------------

const (
	grokCharsetLowerDigits = "abcdefghijklmnopqrstuvwxyz0123456789"
	grokCharsetAlphaNum    = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	grokCharsetUpper       = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	grokCharsetLower       = "abcdefghijklmnopqrstuvwxyz"
)

func grokRandString(r *rand.Rand, charset string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[r.IntN(len(charset))]
	}
	return string(b)
}

func grokNewRand() *rand.Rand {
	return rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().Unix())))
}

// GrokGmailAliasTag builds the unique Gmail +tag used for one farmed account:
// "gk" + (unix seconds mod 100000, NOT zero-padded) + 4 random lower/digits,
// exactly like GmailImapService.create_mailbox.
func GrokGmailAliasTag(unixSec int64, r *rand.Rand) string {
	return fmt.Sprintf("gk%d%s", unixSec%100000, grokRandString(r, grokCharsetLowerDigits, 4))
}

// GrokRandomPassword builds an account password: "GrokFarm" + 8 alphanumerics + "!@".
func GrokRandomPassword(r *rand.Rand) string {
	return "GrokFarm" + grokRandString(r, grokCharsetAlphaNum, 8) + "!@"
}

// GrokRandomDuckUsername builds a DuckMail username: "grok_" + 9 lower/digits.
func GrokRandomDuckUsername(r *rand.Rand) string {
	return "grok_" + grokRandString(r, grokCharsetLowerDigits, 9)
}

// GrokGenerateProfileNames builds the fake profile names: first = "Grok" +
// 1 uppercase + 4 lowercase, last = 1 uppercase + 5 lowercase, mirroring
// fill_profile_and_submit.
func GrokGenerateProfileNames(r *rand.Rand) (first, last string) {
	first = "Grok" + grokRandString(r, grokCharsetUpper, 1) + grokRandString(r, grokCharsetLower, 4)
	last = grokRandString(r, grokCharsetUpper, 1) + grokRandString(r, grokCharsetLower, 5)
	return first, last
}

// ---------------------------------------------------------------------------
// GmailImapService — Gmail +subaddress aliasing + OTP polling over IMAP
// ---------------------------------------------------------------------------

// GmailMailboxService manages disposable Gmail aliases (Google supports
// unlimited sub-addressing: user+tag@gmail.com) and polls the verification
// code via IMAP, mirroring GmailImapService.
//
// Production must set NewEmailClient to a factory returning an IMAP-backed
// EmailClient (e.g. built on github.com/emersion/go-imap). Until then, polls
// fail with errGrokNoIMAPClient.
type GmailMailboxService struct {
	User        string
	AppPassword string

	// Email/Password are set by CreateMailbox.
	Email    string
	Password string

	// Rand is the randomness source (seed it in tests for determinism).
	Rand *rand.Rand
	// PollInterval is the sleep between IMAP poll rounds (default 3s).
	PollInterval time.Duration
	// NewEmailClient builds the IMAP client for one poll round. Production
	// wiring goes here; nil means "not wired" and PollVerificationCode fails.
	NewEmailClient func(ctx context.Context) (EmailClient, error)

	tag string
}

// NewGmailMailboxService returns a Gmail mailbox service for user/appPassword.
func NewGmailMailboxService(user, appPassword string, r *rand.Rand) *GmailMailboxService {
	if r == nil {
		r = grokNewRand()
	}
	return &GmailMailboxService{
		User:         strings.TrimSpace(user),
		AppPassword:  strings.ReplaceAll(appPassword, " ", ""),
		Rand:         r,
		PollInterval: 3 * time.Second,
	}
}

// grokNowUnix is the clock seam for alias minting (stubbed in tests).
var grokNowUnix = func() int64 { return time.Now().Unix() }

// CreateMailbox mints user+gk<tag>@gmail.com and a random password, mirroring
// GmailImapService.create_mailbox.
func (s *GmailMailboxService) CreateMailbox() (email, password string) {
	clean := s.User
	if i := strings.Index(clean, "@"); i >= 0 {
		clean = clean[:i]
	}
	s.tag = GrokGmailAliasTag(grokNowUnix(), s.Rand)
	s.Email = clean + "+" + s.tag + "@gmail.com"
	s.Password = GrokRandomPassword(s.Rand)
	return s.Email, s.Password
}

// Tag returns the alias tag minted by CreateMailbox.
func (s *GmailMailboxService) Tag() string { return s.tag }

// PollVerificationCode polls the INBOX until an OTP email for this mailbox
// arrives or timeout elapses. Mirrors the Python loop: reconnect each round,
// scan ALL, inspect the newest 25 messages, extract the code, sleep between
// rounds. Per-round network/parse errors are swallowed (like the Python bare
// except); timeout returns ("", nil).
func (s *GmailMailboxService) PollVerificationCode(ctx context.Context, timeout time.Duration) (string, error) {
	target := strings.ToLower(strings.TrimSpace(s.Email))
	if target == "" || s.User == "" || s.AppPassword == "" {
		return "", nil
	}
	if s.NewEmailClient == nil {
		return "", errGrokNoIMAPClient
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if code, ok := s.pollOnce(ctx); ok {
			return code, nil
		}
		if !grokSleepCtx(ctx, s.pollIntervalOrDefault()) {
			break
		}
	}
	return "", nil
}

func (s *GmailMailboxService) pollIntervalOrDefault() time.Duration {
	if s.PollInterval <= 0 {
		return 3 * time.Second
	}
	return s.PollInterval
}

// pollOnce runs a single IMAP round and reports (code, found).
func (s *GmailMailboxService) pollOnce(ctx context.Context) (string, bool) {
	client, err := s.NewEmailClient(ctx)
	if err != nil {
		return "", false
	}
	defer func() { _ = client.Close() }()

	ids, err := client.Search(ctx, "ALL")
	if err != nil || len(ids) == 0 {
		return "", false
	}
	target := strings.ToLower(strings.TrimSpace(s.Email))
	start := 0
	if len(ids) > 25 {
		start = len(ids) - 25
	}
	for i := len(ids) - 1; i >= start; i-- {
		msg, err := client.Fetch(ctx, ids[i])
		if err != nil || msg == nil {
			continue
		}
		if !GrokEmailMatchesOTP(*msg, target, s.tag) {
			continue
		}
		if code := ExtractGrokOTP(msg.Subject + " " + msg.Body); code != "" {
			_ = client.MarkRead(ctx, ids[i])
			return code, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// DuckMailService — disposable mailbox via DuckMail / Mail.tm HTTP API
// ---------------------------------------------------------------------------

// DuckMailService manages instant disposable mailboxes via the DuckMail API
// (falling back to Mail.tm), mirroring DuckMailService. Plain HTTPS, so this
// is fully implemented with net/http.
type DuckMailService struct {
	// HTTP is the client for API calls; nil uses GrokDefaultHTTPClient.
	HTTP *http.Client
	// APIBases are tried in order; nil uses the DuckMail → Mail.tm default.
	APIBases []string

	// APIBase is the base that succeeded; Email/Password/Token/AccountID are
	// set by CreateMailbox.
	APIBase   string
	Email     string
	Password  string
	Token     string
	AccountID string

	// Rand is the randomness source (seed it in tests for determinism).
	Rand *rand.Rand
	// PollInterval is the sleep between message-list polls (default 2.5s).
	PollInterval time.Duration
}

// NewDuckMailService returns a DuckMail service using client (or the shared
// default client when nil).
func NewDuckMailService(client *http.Client) *DuckMailService {
	return &DuckMailService{
		HTTP:         client,
		APIBases:     []string{"https://api.duckmail.sbs", "https://api.mail.tm"},
		Rand:         grokNewRand(),
		PollInterval: 2500 * time.Millisecond,
	}
}

func (s *DuckMailService) http() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return GrokDefaultHTTPClient
}

func (s *DuckMailService) bases() []string {
	if len(s.APIBases) > 0 {
		return s.APIBases
	}
	return []string{"https://api.duckmail.sbs", "https://api.mail.tm"}
}

type grokDuckDomain struct {
	Domain     string `json:"domain"`
	IsVerified *bool  `json:"isVerified"`
	IsActive   *bool  `json:"isActive"`
}

// usable mirrors the Python domain filter: a domain counts as usable when
// isVerified OR isActive, with a MISSING key defaulting to true (Python's
// d.get("isVerified", True)).
func (d grokDuckDomain) usable() bool {
	verified, active := true, true
	if d.IsVerified != nil {
		verified = *d.IsVerified
	}
	if d.IsActive != nil {
		active = *d.IsActive
	}
	return verified || active
}

type grokDuckDomainsResp struct {
	Member []grokDuckDomain `json:"hydra:member"`
}

type grokDuckIDResp struct {
	ID any `json:"id"`
}

type grokDuckTokenResp struct {
	Token string `json:"token"`
}

type grokDuckMsgItem struct {
	ID any `json:"id"`
}

type grokDuckMsgsResp struct {
	Member []grokDuckMsgItem `json:"hydra:member"`
}

type grokDuckMsgDetail struct {
	Text    string `json:"text"`
	Intro   string `json:"intro"`
	Subject string `json:"subject"`
}

func grokDuckAnyID(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func (s *DuckMailService) getJSON(ctx context.Context, url string, timeout time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("grokfarm: GET %s: unexpected status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *DuckMailService) postJSON(ctx context.Context, url string, timeout time.Duration, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("grokfarm: POST %s: unexpected status %d", url, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// CreateMailbox creates a disposable mailbox on the first working API base:
// pick a verified domain, POST /accounts, then POST /token. Returns the
// address and password, mirroring DuckMailService.create_mailbox (which
// raises when every base fails).
func (s *DuckMailService) CreateMailbox(ctx context.Context) (string, string, error) {
	var lastErr error
	for _, base := range s.bases() {
		email, password, err := s.createOnBase(ctx, base)
		if err == nil {
			s.APIBase = base
			return email, password, nil
		}
		lastErr = err
	}
	return "", "", fmt.Errorf("grokfarm: failed to create mailbox on DuckMail or Mail.tm: %w", lastErr)
}

func (s *DuckMailService) createOnBase(ctx context.Context, base string) (string, string, error) {
	var domains grokDuckDomainsResp
	if err := s.getJSON(ctx, base+"/domains", 6*time.Second, &domains); err != nil {
		return "", "", err
	}
	var usable []string
	for _, d := range domains.Member {
		if d.Domain != "" && d.usable() {
			usable = append(usable, d.Domain)
		}
	}
	if len(usable) == 0 {
		return "", "", fmt.Errorf("grokfarm: no usable domains at %s", base)
	}
	chosen := usable[s.Rand.IntN(len(usable))]
	email := GrokRandomDuckUsername(s.Rand) + "@" + chosen
	password := GrokRandomPassword(s.Rand)

	var acc grokDuckIDResp
	if err := s.postJSON(ctx, base+"/accounts", 7*time.Second,
		map[string]string{"address": email, "password": password}, &acc); err != nil {
		return "", "", err
	}
	var tok grokDuckTokenResp
	if err := s.postJSON(ctx, base+"/token", 7*time.Second,
		map[string]string{"address": email, "password": password}, &tok); err != nil {
		return "", "", err
	}
	if tok.Token == "" {
		return "", "", fmt.Errorf("grokfarm: empty token from %s", base)
	}
	s.Email = email
	s.Password = password
	s.AccountID = grokDuckAnyID(acc.ID)
	s.Token = tok.Token
	return email, password, nil
}

// PollVerificationCode polls /messages until a message containing a code
// arrives or timeout elapses, mirroring DuckMailService.poll_verification_code
// (first message in the list, code hunted across text+intro+subject, 2.5s
// between rounds). Returns ("", nil) on timeout.
func (s *DuckMailService) PollVerificationCode(ctx context.Context, timeout time.Duration) (string, error) {
	if s.Token == "" {
		return "", errors.New("grokfarm: mailbox token not initialized; call CreateMailbox first")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if code := s.pollOnce(ctx); code != "" {
			return code, nil
		}
		if !grokSleepCtx(ctx, s.pollIntervalOrDefault()) {
			break
		}
	}
	return "", nil
}

func (s *DuckMailService) pollIntervalOrDefault() time.Duration {
	if s.PollInterval <= 0 {
		return 2500 * time.Millisecond
	}
	return s.PollInterval
}

func (s *DuckMailService) pollOnce(ctx context.Context) string {
	var list grokDuckMsgsResp
	if err := s.getJSON(ctx, s.APIBase+"/messages", 5*time.Second, &list); err != nil {
		return ""
	}
	if len(list.Member) == 0 {
		return ""
	}
	id := grokDuckAnyID(list.Member[0].ID)
	if id == "" {
		return ""
	}
	var detail grokDuckMsgDetail
	if err := s.getJSON(ctx, s.APIBase+"/messages/"+id, 5*time.Second, &detail); err != nil {
		return ""
	}
	return ExtractDuckMailCode(detail.Text + " " + detail.Intro + " " + detail.Subject)
}

// ---------------------------------------------------------------------------
// Browser automation seam
// ---------------------------------------------------------------------------

// GrokBrowserDriver is the browser automation seam replacing DrissionPage's
// Chromium/tab object. Production implements it on top of chromedp or a raw
// CDP WebSocket client:
//
//   - Navigate loads a URL (page.get).
//   - RunJS executes a script with arguments (page.run_js) and returns the
//     script's return value (bool, string, or nil).
//   - Cookies returns the session cookies (page.cookies).
//   - Close quits the browser (browser.quit).
type GrokBrowserDriver interface {
	Navigate(ctx context.Context, url string) error
	RunJS(ctx context.Context, script string, args ...any) (any, error)
	Cookies(ctx context.Context) ([]GrokCookie, error)
	Close() error
}

// GrokBrowserOptions describes how to launch the signup browser.
type GrokBrowserOptions struct {
	Headless   bool
	WindowSize string // "1920,1080" when headless
	Proxy      string // effective --proxy-server value, "" for none
	UserAgent  string
	ExtraArgs  []string // chromium flags incl. --proxy-server when set
	Extensions []string // e.g. turnstilePatch dir
}

// GrokBrowserFactory launches a browser for one registration attempt.
type GrokBrowserFactory func(ctx context.Context, opts GrokBrowserOptions) (GrokBrowserDriver, error)

// GrokProxyPreparer converts a raw residential proxy line into a Chromium
// --proxy-server value (e.g. via the sibling proxy_bridge port that turns an
// authenticated proxy into a local forward). It returns the effective value
// and an optional stop func (nil when nothing needs stopping).
type GrokProxyPreparer func(ctx context.Context, chosenProxy string) (effectiveProxy string, stop func(), err error)

// GrokBuildBrowserOptions builds the launch options for one registration,
// mirroring the ChromiumOptions setup in register_single_grok_account.
// turnstileExt is "" when the extension dir was not found.
func GrokBuildBrowserOptions(cfg GrokRegisterConfig, effectiveProxy, turnstileExt string) GrokBrowserOptions {
	args := []string{
		"--disable-blink-features=AutomationControlled",
		"--no-sandbox",
		"--disable-dev-shm-usage",
	}
	window := ""
	if cfg.Headless {
		window = "1920,1080"
		args = append(args, "--window-size=1920,1080")
	} else {
		args = append(args, "--start-maximized")
	}
	if effectiveProxy != "" {
		args = append(args, "--proxy-server="+effectiveProxy)
	}
	var exts []string
	if turnstileExt != "" {
		exts = []string{turnstileExt}
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = GrokChromeUserAgent
	}
	return GrokBrowserOptions{
		Headless:   cfg.Headless,
		WindowSize: window,
		Proxy:      effectiveProxy,
		UserAgent:  ua,
		ExtraArgs:  args,
		Extensions: exts,
	}
}

// GrokDefaultTurnstilePaths returns the two extension-dir candidates the
// Python original probes: <coreDir>/turnstilePatch, then
// <coreDir>/../../grok-register/turnstilePatch.
func GrokDefaultTurnstilePaths(coreDir string) []string {
	return []string{
		filepath.Join(coreDir, "turnstilePatch"),
		filepath.Join(coreDir, "..", "..", "grok-register", "turnstilePatch"),
	}
}

// GrokFindTurnstileExtension returns the first existing directory in paths,
// or "" when none exists.
func GrokFindTurnstileExtension(paths []string) string {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
	}
	return ""
}

// GrokCheckProxyGateway mirrors the Python TCP reachability probe for
// proxy_gateway (http://host:port, 200ms connect timeout): true when the
// gateway accepts a TCP connection.
func GrokCheckProxyGateway(gateway string) bool {
	host := strings.TrimPrefix(strings.TrimPrefix(gateway, "http://"), "https://")
	parts := strings.Split(host, ":")
	if len(parts) != 2 || parts[0] == "" {
		return false
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil || port <= 0 || port > 65535 {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(parts[0], parts[1]), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ---------------------------------------------------------------------------
// Signup form JS payloads (ported verbatim from grok_farm.py)
// ---------------------------------------------------------------------------

// GrokClickEmailSignupJS detects and clicks the "Sign up with email" /
// "Continue with email" button on the xAI landing page. Returns the button
// text (or true) on success, false when nothing clickable is found.
const GrokClickEmailSignupJS = `
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
function nodeText(node) {
    return [
        node.innerText,
        node.textContent,
        node.getAttribute('aria-label'),
        node.getAttribute('title'),
        node.getAttribute('href'),
    ].filter(Boolean).join(' ').replace(/\s+/g, ' ').trim();
}
function scoreEntry(node) {
    const compact = nodeText(node).replace(/\s+/g, '');
    const lower = compact.toLowerCase();
    if (compact.includes('使用邮箱注册')) return 100;
    if (lower.includes('signupwithemail')) return 95;
    if (lower.includes('continuewithemail')) return 90;
    if (lower.includes('email') && (lower.includes('sign') || lower.includes('continue') || lower.includes('use') || lower.includes('with'))) return 80;
    if (lower === 'email' || lower.includes('邮箱') || lower.includes('sign up with email')) return 75;
    return 0;
}
const candidates = Array.from(document.querySelectorAll('button, a, [role="button"]'))
    .filter((node) => isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true')
    .map((node) => ({ node, score: scoreEntry(node), text: nodeText(node) }))
    .filter((item) => item.score > 0)
    .sort((a, b) => b.score - a.score);
const target = candidates[0]?.node || null;
if (!target) return false;
target.click();
return candidates[0].text || true;
`

// GrokFillEmailJS fills the email input with natural typing events and
// submits. Takes the email as arguments[0]. Returns 'clicked', 'enter',
// 'reclicked', or false.
const GrokFillEmailJS = `
const email = arguments[0];
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
const inputs = Array.from(document.querySelectorAll('input[type="email"], input[name="email"], input[data-testid="email"], input[placeholder*="email" i], input[autocomplete="email"], input'))
    .filter(node => isVisible(node) && !node.disabled && !node.readOnly);
const input = inputs[0];
if (!input) {
    const btns = Array.from(document.querySelectorAll('button, a, [role="button"]'))
        .filter(node => isVisible(node) && !node.disabled);
    const signupBtn = btns.find(b => {
        const t = (b.innerText || b.textContent || '').toLowerCase();
        return t.includes('sign up with email') || t.includes('continue with email') || (t.includes('email') && t.includes('sign'));
    });
    if (signupBtn) {
        signupBtn.click();
        return 'reclicked';
    }
    return false;
}

input.focus();
input.click();
const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
const tracker = input._valueTracker;
if (tracker) tracker.setValue('');
if (nativeSetter) nativeSetter.call(input, email);
else input.value = email;

input.dispatchEvent(new InputEvent('beforeinput', { bubbles: true, data: email, inputType: 'insertText' }));
input.dispatchEvent(new InputEvent('input', { bubbles: true, data: email, inputType: 'insertText' }));
input.dispatchEvent(new Event('change', { bubbles: true }));

const buttons = Array.from(document.querySelectorAll('button[type="submit"], button, [role="button"]'))
    .filter(node => isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true');
const btn = buttons.find(b => {
    const t = (b.innerText || b.textContent || '').toLowerCase();
    return t.includes('continue') || t.includes('sign up') || t.includes('next') || t.includes('lanjut') || t.includes('submit');
});
if (btn) {
    btn.click();
    return 'clicked';
}
input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', code: 'Enter', bubbles: true }));
return 'enter';
`

// GrokFillOTPJS fills the 6-digit verification code. Takes the code as
// arguments[0]. Returns 'aggregate-filled', 'boxes-filled', or false.
const GrokFillOTPJS = `
const code = String(arguments[0] || '').trim();
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
function setInputValue(input, value) {
    const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
    const tracker = input._valueTracker;
    if (tracker) tracker.setValue('');
    if (nativeSetter) nativeSetter.call(input, value);
    else input.value = value;
    input.dispatchEvent(new InputEvent('beforeinput', { bubbles: true, data: value, inputType: 'insertText' }));
    input.dispatchEvent(new InputEvent('input', { bubbles: true, data: value, inputType: 'insertText' }));
    input.dispatchEvent(new Event('change', { bubbles: true }));
}

const aggregate = Array.from(document.querySelectorAll('input[data-input-otp="true"], input[name="code"], input[autocomplete="one-time-code"], input[maxlength="6"], input[inputmode="numeric"]'))
    .find(node => isVisible(node) && !node.disabled && !node.readOnly && Number(node.maxLength || 6) > 1);

if (aggregate) {
    aggregate.focus();
    setInputValue(aggregate, code);
    return 'aggregate-filled';
}

const otpBoxes = Array.from(document.querySelectorAll('input')).filter(node => {
    if (!isVisible(node) || node.disabled || node.readOnly) return false;
    const maxLength = Number(node.maxLength || 0);
    return maxLength === 1 || String(node.autocomplete || '').toLowerCase() === 'one-time-code';
});

if (otpBoxes.length >= code.length) {
    for (let i = 0; i < code.length; i++) {
        const ch = code[i];
        const box = otpBoxes[i];
        box.focus();
        setInputValue(box, ch);
        box.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, key: ch }));
        box.dispatchEvent(new KeyboardEvent('keyup', { bubbles: true, key: ch }));
    }
    return 'boxes-filled';
}
return false;
`

// GrokFillOTPSubmitJS clicks the verify/continue button after the OTP was
// filled (the second run_js in fill_otp_and_submit).
const GrokFillOTPSubmitJS = `
function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
const buttons = Array.from(document.querySelectorAll('button[type="submit"], button, [role="button"]'))
    .filter(node => isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true');
const btn = buttons.find(b => {
    const t = (b.innerText || b.textContent || '').toLowerCase();
    return t.includes('verify') || t.includes('continue') || t.includes('next') || t.includes('confirm');
});
if (btn) btn.click();
`

// GrokFillProfileJS fills first/last name (or full name) and the password,
// then clicks the agree/create/continue button. Takes first, last, password
// as arguments[0..2]. Returns true, or whether a name input existed.
const GrokFillProfileJS = `
const first = arguments[0];
const last = arguments[1];
const pwd = arguments[2];

function isVisible(node) {
    if (!node) return false;
    const style = window.getComputedStyle(node);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    const rect = node.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
}
function setVal(node, val) {
    if (!node) return;
    node.focus();
    const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
    const tracker = node._valueTracker;
    if (tracker) tracker.setValue('');
    if (nativeSetter) nativeSetter.call(node, val);
    else node.value = val;
    node.dispatchEvent(new InputEvent('input', { bubbles: true, data: val }));
    node.dispatchEvent(new Event('change', { bubbles: true }));
}

const givenInp = document.querySelector('input[data-testid="givenName"], input[name="givenName"], input[autocomplete="given-name"], input[placeholder*="First" i], input[placeholder*="Nama" i]');
const familyInp = document.querySelector('input[data-testid="familyName"], input[name="familyName"], input[autocomplete="family-name"], input[placeholder*="Last" i]');
const nameInp = document.querySelector('input[name="name"], input[placeholder*="Name" i]');
const pwdInp = document.querySelector('input[type="password"], input[data-testid="password"], input[name="password"]');

if (givenInp && familyInp) {
    setVal(givenInp, first);
    setVal(familyInp, last);
} else if (nameInp) {
    setVal(nameInp, first + ' ' + last);
}

if (pwdInp) {
    setVal(pwdInp, pwd);
}

const buttons = Array.from(document.querySelectorAll('button[type="submit"], button, [role="button"]'))
    .filter(node => isVisible(node) && !node.disabled && node.getAttribute('aria-disabled') !== 'true');
const btn = buttons.find(b => {
    const t = (b.innerText || b.textContent || '').toLowerCase();
    return t.includes('agree') || t.includes('create') || t.includes('continue') || t.includes('finish') || t.includes('signup') || t.includes('sign up');
});
if (btn) {
    btn.click();
    return true;
}
return !!(givenInp || nameInp);
`

// ---------------------------------------------------------------------------
// Signup form step helpers (retry loops)
// ---------------------------------------------------------------------------

func grokRetryForm(ctx context.Context, timeout, interval time.Duration, attempt func(ctx context.Context) (bool, error)) bool {
	deadline := time.Now().Add(timeout)
	for {
		done, err := attempt(ctx)
		if err == nil && done {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return false
		}
		wait := interval
		if wait > remain {
			wait = remain
		}
		if !grokSleepCtx(ctx, wait) {
			return false
		}
	}
}

func grokSleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// grokJSTruthy interprets a RunJS return value with JS truthiness semantics.
func grokJSTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		s := strings.TrimSpace(t)
		return s != "" && s != "false"
	case float64:
		return t != 0
	default:
		return true
	}
}

func grokJSToString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// GrokClickEmailSignupButton detects and clicks the "Sign up with email"
// button, retrying until timeout. Mirrors click_email_signup_button.
func GrokClickEmailSignupButton(ctx context.Context, d GrokBrowserDriver, timeout time.Duration) bool {
	return grokRetryForm(ctx, timeout, 500*time.Millisecond, func(ctx context.Context) (bool, error) {
		v, err := d.RunJS(ctx, GrokClickEmailSignupJS)
		if err != nil {
			return false, err
		}
		return grokJSTruthy(v), nil
	})
}

// GrokFillEmailAndSubmit fills the email input and submits, retrying until
// timeout. True only when the JS reports 'clicked' or 'enter', mirroring
// fill_email_and_submit.
func GrokFillEmailAndSubmit(ctx context.Context, d GrokBrowserDriver, email string, timeout time.Duration) bool {
	return grokRetryForm(ctx, timeout, time.Second, func(ctx context.Context) (bool, error) {
		v, err := d.RunJS(ctx, GrokFillEmailJS, email)
		if err != nil {
			return false, err
		}
		res := grokJSToString(v)
		return res == "clicked" || res == "enter", nil
	})
}

// GrokFillOTPAndSubmit fills the OTP code then clicks the verify button,
// mirroring fill_otp_and_submit (including the 1s pause before clicking).
func GrokFillOTPAndSubmit(ctx context.Context, d GrokBrowserDriver, otp string, timeout time.Duration) bool {
	filled := grokRetryForm(ctx, timeout, 500*time.Millisecond, func(ctx context.Context) (bool, error) {
		v, err := d.RunJS(ctx, GrokFillOTPJS, otp)
		if err != nil {
			return false, err
		}
		return grokJSTruthy(v), nil
	})
	if !filled {
		return false
	}
	grokSleepCtx(ctx, time.Second)
	_, _ = d.RunJS(ctx, GrokFillOTPSubmitJS)
	return true
}

// GrokFillProfileAndSubmit fills the generated profile names and the account
// password, then clicks the agree/create button, mirroring
// fill_profile_and_submit.
func GrokFillProfileAndSubmit(ctx context.Context, d GrokBrowserDriver, password string, r *rand.Rand, timeout time.Duration) bool {
	if r == nil {
		r = grokNewRand()
	}
	first, last := GrokGenerateProfileNames(r)
	return grokRetryForm(ctx, timeout, 500*time.Millisecond, func(ctx context.Context) (bool, error) {
		v, err := d.RunJS(ctx, GrokFillProfileJS, first, last, password)
		if err != nil {
			return false, err
		}
		return grokJSTruthy(v), nil
	})
}

// ---------------------------------------------------------------------------
// Single-account registration
// ---------------------------------------------------------------------------

// GrokRegisterDelays holds the fixed pauses between signup steps. The
// defaults mirror the time.sleep calls in register_single_grok_account;
// tests can zero them.
type GrokRegisterDelays struct {
	Open         time.Duration // after page load (default 4s)
	AfterClick   time.Duration // after clicking "sign up with email" (default 1s)
	AfterOTP     time.Duration // after filling the OTP (default 2.5s)
	AfterProfile time.Duration // after filling the profile (default 3s)
}

// DefaultGrokRegisterDelays returns the production delays.
func DefaultGrokRegisterDelays() GrokRegisterDelays {
	return GrokRegisterDelays{
		Open:         4 * time.Second,
		AfterClick:   time.Second,
		AfterOTP:     2500 * time.Millisecond,
		AfterProfile: 3 * time.Second,
	}
}

// GrokRegisterConfig configures one registration attempt.
type GrokRegisterConfig struct {
	Index int
	Total int

	Headless     bool
	ProxyGateway string // e.g. "http://127.0.0.1:8888"; used when reachable
	MailProvider string // "gmail" (default) or "duckmail"

	GmailUser        string
	GmailAppPassword string
	UserAgent        string // default GrokChromeUserAgent

	ProxyBaseDir      string   // base dir for FindResidentialProxies; "" = "."
	TurnstileExtPaths []string // candidates for GrokFindTurnstileExtension

	HTTPClient     *http.Client
	NewBrowser     GrokBrowserFactory // REQUIRED: chromedp/CDP factory
	NewEmailClient func(ctx context.Context) (EmailClient, error)
	PrepareProxy   GrokProxyPreparer // optional: raw proxy line -> effective --proxy-server

	Rng    *rand.Rand
	Delays GrokRegisterDelays // zero value = DefaultGrokRegisterDelays()
	State  *GrokFarmState     // nil = throwaway state
}

func (c GrokRegisterConfig) mailProvider() string {
	if strings.EqualFold(strings.TrimSpace(c.MailProvider), "duckmail") {
		return "duckmail"
	}
	return "gmail"
}

func (c GrokRegisterConfig) delaysOrDefault() GrokRegisterDelays {
	d := c.Delays
	if d.Open == 0 && d.AfterClick == 0 && d.AfterOTP == 0 && d.AfterProfile == 0 {
		return DefaultGrokRegisterDelays()
	}
	return d
}

// RegisterSingleGrokAccount runs the full signup flow for one account:
// pick a residential proxy, mint a mailbox, drive the browser through the
// xAI signup form, poll the OTP, fill the profile, and extract the SSO
// cookies. Mirrors register_single_grok_account.
//
// Returns (nil, nil) when the attempt fails gracefully (the failure is
// logged, like the Python returning None); a non-nil error is reserved for
// misconfiguration (e.g. NewBrowser not wired).
func RegisterSingleGrokAccount(ctx context.Context, cfg GrokRegisterConfig) (*GrokAccount, error) {
	st := cfg.State
	if st == nil {
		st = NewGrokFarmState()
	}
	rng := cfg.Rng
	if rng == nil {
		rng = grokNewRand()
	}
	delay := cfg.delaysOrDefault()
	if cfg.NewBrowser == nil {
		return nil, errors.New("grokfarm: GrokRegisterConfig.NewBrowser is nil; wire a chromedp/CDP GrokBrowserFactory (see package docs)")
	}

	// 1. Residential proxy pick.
	baseDir := cfg.ProxyBaseDir
	if baseDir == "" {
		baseDir = "."
	}
	residential := FindResidentialProxies(baseDir)
	chosen := ""
	if len(residential) > 0 {
		chosen = residential[rng.IntN(len(residential))]
	}
	if chosen != "" {
		display := chosen
		if i := strings.LastIndex(display, "@"); i >= 0 {
			display = display[i+1:]
		}
		st.Log("Menggunakan Proxy Residential: "+display, GrokLogInfo, "")
	}

	// 2. Mailbox.
	var emailAddr, password string
	var gmailSvc *GmailMailboxService
	var duckSvc *DuckMailService
	if cfg.mailProvider() == "duckmail" {
		duckSvc = NewDuckMailService(cfg.HTTPClient)
		duckSvc.Rand = rng
		st.Log(fmt.Sprintf("Membuat disposable mailbox via DuckMail/Mail.tm (Akun [%d/%d])...", cfg.Index, cfg.Total),
			GrokLogInfo, GrokStepCreatingEmail)
		var err error
		emailAddr, password, err = duckSvc.CreateMailbox(ctx)
		if err != nil {
			st.Log("Gagal membuat email: "+err.Error(), GrokLogError, "")
			return nil, nil
		}
	} else {
		gmailSvc = NewGmailMailboxService(cfg.GmailUser, cfg.GmailAppPassword, rng)
		gmailSvc.NewEmailClient = cfg.NewEmailClient
		st.Log(fmt.Sprintf("Membuat alias Gmail via IMAP (Akun [%d/%d])...", cfg.Index, cfg.Total),
			GrokLogInfo, GrokStepCreatingEmail)
		emailAddr, password = gmailSvc.CreateMailbox()
	}
	st.Log("Mailbox Aktif: "+emailAddr+" | Password: "+password, GrokLogSuccess, GrokStepOpeningBrowser)

	// 3. Effective proxy: gateway when reachable, else prepared residential line.
	effectiveProxy := ""
	if cfg.ProxyGateway != "" && GrokCheckProxyGateway(cfg.ProxyGateway) {
		effectiveProxy = cfg.ProxyGateway
	} else if chosen != "" {
		if cfg.PrepareProxy != nil {
			ep, stop, err := cfg.PrepareProxy(ctx, chosen)
			if stop != nil {
				defer stop()
			}
			if err != nil {
				effectiveProxy = chosen
			} else {
				effectiveProxy = ep
			}
		} else {
			effectiveProxy = chosen
		}
	}

	// 4. Browser launch.
	ext := GrokFindTurnstileExtension(cfg.TurnstileExtPaths)
	drv, err := cfg.NewBrowser(ctx, GrokBuildBrowserOptions(cfg, effectiveProxy, ext))
	if err != nil {
		st.Log("Exception registrasi Grok: "+err.Error(), GrokLogError, "")
		return nil, nil
	}
	defer func() { _ = drv.Close() }()

	st.Log("Membuka halaman registrasi Grok xAI: "+GrokSignupURL, GrokLogInfo, GrokStepOpeningBrowser)
	if err := drv.Navigate(ctx, GrokSignupURL); err != nil {
		st.Log("Exception registrasi Grok: "+err.Error(), GrokLogError, "")
		return nil, nil
	}
	grokSleepCtx(ctx, delay.Open)

	// 5. Click "Sign up with email".
	st.Log("Mencari & mengklik tombol 'Sign up with email'...", GrokLogInfo, GrokStepEnteringEmail)
	if GrokClickEmailSignupButton(ctx, drv, 8*time.Second) {
		st.Log("Berhasil masuk ke form input email.", GrokLogDebug, "")
	}
	grokSleepCtx(ctx, delay.AfterClick)

	// 6. Submit email.
	st.Log(fmt.Sprintf("Memasukkan email (%s) dan submit...", emailAddr), GrokLogInfo, GrokStepEnteringEmail)
	if !GrokFillEmailAndSubmit(ctx, drv, emailAddr, 12*time.Second) {
		st.Log("Gagal submit email ke form registrasi.", GrokLogWarning, "")
	}

	// 7. Poll OTP.
	pollSource := "Gmail IMAP Inbox"
	if duckSvc != nil && duckSvc.APIBase != "" {
		pollSource = duckSvc.APIBase
	}
	st.Log(fmt.Sprintf("Menunggu kode OTP 6-digit dari xAI (polling %s)...", pollSource), GrokLogInfo, GrokStepWaitingOTP)
	var otp string
	if duckSvc != nil {
		otp, _ = duckSvc.PollVerificationCode(ctx, 90*time.Second)
	} else {
		otp, _ = gmailSvc.PollVerificationCode(ctx, 90*time.Second)
	}
	if otp == "" {
		st.Log("Waktu tunggu OTP habis (Timeout 90s).", GrokLogError, "")
		return nil, nil
	}
	st.Log("KODE OTP DITERIMA: "+otp+"!", GrokLogSuccess, GrokStepVerifyingOTP)

	// 8. Fill OTP.
	st.Log(fmt.Sprintf("Mengisi kode OTP (%s) ke form verifikasi...", otp), GrokLogInfo, GrokStepVerifyingOTP)
	GrokFillOTPAndSubmit(ctx, drv, otp, 10*time.Second)
	grokSleepCtx(ctx, delay.AfterOTP)

	// 9. Profile.
	st.Log("Mengisi data profil & menyetujui Ketentuan Layanan...", GrokLogInfo, GrokStepSettingProfile)
	GrokFillProfileAndSubmit(ctx, drv, password, rng, 10*time.Second)
	grokSleepCtx(ctx, delay.AfterProfile)

	// 10. Extract SSO cookies.
	st.Log("Mengekstrak token autentikasi & session cookies...", GrokLogInfo, GrokStepFinalizing)
	cookies, _ := drv.Cookies(ctx)
	ssoToken := ""
	for _, c := range cookies {
		if c.Name == "sso" || c.Name == "sso-rw" || c.Name == "__Secure-next-auth.session-token" {
			ssoToken = c.Value
			break
		}
	}

	acc := &GrokAccount{
		Email:     emailAddr,
		Password:  password,
		SSOToken:  ssoToken,
		Cookies:   cookies,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}
	st.Log(fmt.Sprintf("SUKSES! Akun Grok xAI [%d/%d] berhasil dipanen: %s", cfg.Index, cfg.Total, emailAddr),
		GrokLogSuccess, GrokStepCompleted)
	return acc, nil
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// SaveGrokAccount appends the account to output/grok_accounts.txt (as
// "email----password----sso_token", NO_SSO_COOKIE when empty) and to the JSON
// array in output/grok_accounts.json, mirroring save_grok_account. An empty
// outputDir defaults to "output".
func SaveGrokAccount(account GrokAccount, outputDir string) error {
	if outputDir == "" {
		outputDir = "output"
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("grokfarm: create output dir: %w", err)
	}
	sso := account.SSOToken
	if sso == "" {
		sso = "NO_SSO_COOKIE"
	}
	txtPath := filepath.Join(outputDir, "grok_accounts.txt")
	f, err := os.OpenFile(txtPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("grokfarm: append %s: %w", txtPath, err)
	}
	if _, err := fmt.Fprintf(f, "%s----%s----%s\n", account.Email, account.Password, sso); err != nil {
		_ = f.Close()
		return fmt.Errorf("grokfarm: write %s: %w", txtPath, err)
	}
	_ = f.Close()

	jsonPath := filepath.Join(outputDir, "grok_accounts.json")
	var existing []GrokAccount
	if data, err := os.ReadFile(jsonPath); err == nil {
		_ = json.Unmarshal(data, &existing) // corrupt file -> start fresh, like the Python
	}
	existing = append(existing, account)
	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("grokfarm: marshal accounts: %w", err)
	}
	if err := os.WriteFile(jsonPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("grokfarm: write %s: %w", jsonPath, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Farm runner
// ---------------------------------------------------------------------------

// GrokFarmConfig configures a full farm run.
type GrokFarmConfig struct {
	Total int // target account count; <1 treated as 1

	Headless     bool
	ProxyGateway string // e.g. "http://127.0.0.1:8888"
	MailProvider string // "gmail" (default) or "duckmail"

	GmailUser        string
	GmailAppPassword string
	UserAgent        string

	ProxyBaseDir      string
	TurnstileExtPaths []string
	OutputDir         string // default "output"

	HTTPClient     *http.Client
	NewBrowser     GrokBrowserFactory
	NewEmailClient func(ctx context.Context) (EmailClient, error)
	PrepareProxy   GrokProxyPreparer

	Rng *rand.Rand
	// BetweenAccounts is the pause between accounts (default 3s).
	BetweenAccounts time.Duration
	RegisterDelays  GrokRegisterDelays

	// State receives progress/logs; nil = throwaway state.
	State *GrokFarmState
	// Register is the per-account seam; nil = RegisterSingleGrokAccount.
	// Tests inject a fake here to exercise RunGrokFarm without a browser.
	Register func(ctx context.Context, cfg GrokRegisterConfig) (*GrokAccount, error)
}

// RunGrokFarm harvests Total accounts sequentially, persisting each and
// updating the shared state, mirroring run_grok_farm.
func RunGrokFarm(ctx context.Context, cfg GrokFarmConfig) ([]GrokAccount, error) {
	total := cfg.Total
	if total < 1 {
		total = 1
	}
	st := cfg.State
	if st == nil {
		st = NewGrokFarmState()
	}
	register := cfg.Register
	if register == nil {
		register = RegisterSingleGrokAccount
	}
	between := cfg.BetweenAccounts
	if between <= 0 {
		between = 3 * time.Second
	}
	rng := cfg.Rng
	if rng == nil {
		rng = grokNewRand()
	}

	st.begin(total)
	provider := strings.ToUpper(cfg.mailProviderOr("gmail"))
	st.Log(fmt.Sprintf("Memulai Mesin Ternak Grok xAI (Target: %d akun, Headless: %v, Provider: %s)",
		total, cfg.Headless, provider), GrokLogInfo, "")

	var harvested []GrokAccount
	for i := 1; i <= total; i++ {
		if ctx.Err() != nil {
			break
		}
		st.setCurrent(i, int(float64(i-1)/float64(total)*90)+10)

		rcfg := GrokRegisterConfig{
			Index:             i,
			Total:             total,
			Headless:          cfg.Headless,
			ProxyGateway:      cfg.ProxyGateway,
			MailProvider:      cfg.MailProvider,
			GmailUser:         cfg.GmailUser,
			GmailAppPassword:  cfg.GmailAppPassword,
			UserAgent:         cfg.UserAgent,
			ProxyBaseDir:      cfg.ProxyBaseDir,
			TurnstileExtPaths: cfg.TurnstileExtPaths,
			HTTPClient:        cfg.HTTPClient,
			NewBrowser:        cfg.NewBrowser,
			NewEmailClient:    cfg.NewEmailClient,
			PrepareProxy:      cfg.PrepareProxy,
			Rng:               rng,
			Delays:            cfg.RegisterDelays,
			State:             st,
		}
		acc, err := register(ctx, rcfg)
		if err != nil {
			st.Log("Exception registrasi Grok: "+err.Error(), GrokLogError, "")
		}
		switch {
		case acc != nil:
			if err := SaveGrokAccount(*acc, cfg.OutputDir); err != nil {
				st.Log("Gagal menyimpan akun: "+err.Error(), GrokLogError, "")
			} else {
				harvested = append(harvested, *acc)
				st.addHarvested(*acc)
			}
		default:
			st.Log(fmt.Sprintf("Akun [%d/%d] gagal diproses.", i, total), GrokLogWarning, "")
		}

		if i < total {
			grokSleepCtx(ctx, between)
		}
	}

	st.finish(len(harvested), total)
	return harvested, nil
}

func (c GrokFarmConfig) mailProviderOr(def string) string {
	if strings.EqualFold(strings.TrimSpace(c.MailProvider), "duckmail") {
		return "duckmail"
	}
	if strings.EqualFold(strings.TrimSpace(c.MailProvider), "gmail") {
		return "gmail"
	}
	return def
}
