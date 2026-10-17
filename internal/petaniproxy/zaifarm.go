// Z.ai account farm orchestrator: a Go port of petani-proxy
// core/zai_farm.py ("Mesin Ternak Akun Z.ai / AutoClaw").
//
// The Python original drives Chromium through DrissionPage to register accounts
// at https://chat.z.ai/auth with disposable mailboxes, solves the signup flow,
// captures session cookies as tokens, and persists harvested accounts to the
// AutoClawPi SQLite database plus output/zai_accounts.txt.
//
// Porting notes (Python -> Go):
//   - DrissionPage/Chromium has no stdlib equivalent, so browser automation is
//     an injected seam: ZaiBrowserDriver. The orchestration, xpaths, sleeps,
//     retry/backoff and state machine are ported 1:1.
//   - The module-global zai_farm_state dict + threading.Lock becomes ZaiFarm
//     with a sync.Mutex; logs are a ring buffer capped at 60 entries.
//   - SQLite persistence (save_to_autoclawpi_db) is an injected seam:
//     ZaiAccountStore. The stdlib has no SQLite driver; production wiring uses
//     modernc.org/sqlite and ZaiAutoClawDBPaths for the candidate DB locations.
//   - OTP polling, mailbox creation and all pure helpers live in zaifarm_mail.go.
package petaniproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ZaiAuthURL mirrors ZAI_AUTH_URL, the official Z.ai registration page.
const ZaiAuthURL = "https://chat.z.ai/auth"

// DefaultZaiAccountsPath is the harvest output file, relative to the
// petani-proxy module root, mirroring output/zai_accounts.txt.
const DefaultZaiAccountsPath = "output/zai_accounts.txt"

// Log levels mirroring the level argument of zai_log().
const (
	ZaiLogInfo    = "info"
	ZaiLogSuccess = "success"
	ZaiLogWarning = "warning"
	ZaiLogError   = "error"
	ZaiLogDebug   = "debug"
)

// zaiMaxLogEntries mirrors the 60-entry cap on zai_farm_state["logs"].
const zaiMaxLogEntries = 60

// Signup form selectors, verbatim from run_zai_farm's tab.ele() calls.
const (
	zaiXPathNameInput   = `//input[@placeholder="Enter Your Full Name" or @name="name"]`
	zaiXPathEmailInput  = `//input[@placeholder="Enter Your Email" or @type="email" or @name="email"]`
	zaiXPathPassInput   = `//input[@placeholder="Enter Your Password" or @type="password" or @name="password"]`
	zaiXPathCaptchaBtn  = `//div[contains(text(), "Click to start verification")] | //button[contains(text(), "verification")]`
	zaiXPathSubmitBtn   = `//button[contains(text(), "Create Account") or contains(text(), "Sign Up")]`
	zaiXPathOTPInput    = `//input[@placeholder="Enter OTP" or @placeholder="Code" or @name="code"]`
	zaiXPathOTPConfirm  = `//button[contains(text(), "Verify") or contains(text(), "Confirm")]`
)

// ZaiBrowserDriver is the seam for browser automation, replacing DrissionPage's
// Chromium/tab API. Fill/Click return found=false when no element matches the
// xpath, mirroring Python's `if name_input:` guards (missing elements are
// skipped, not errors).
//
// Production wiring: implement with chromedp or rod over a real Chromium:
//   - Navigate: chromedp.Navigate / page.Navigate
//   - Fill:     chromedp.SendKeys on the first xpath match (chromedp.BySearch)
//   - Click:    chromedp.Click on the first xpath match
//   - Has:      presence probe (chromedp.WaitVisible with a short timeout)
//   - Cookies:  network.GetCookies for the tab's domain
//   - Close:    cancel the chromedp context / browser.Close()
type ZaiBrowserDriver interface {
	// Navigate loads url in the current tab.
	Navigate(ctx context.Context, url string) error
	// Fill types value into the first element matching xpath.
	// found=false means no element matched (skipped, like Python's None guard).
	Fill(ctx context.Context, xpath, value string) (found bool, err error)
	// Click clicks the first element matching xpath.
	Click(ctx context.Context, xpath string) (found bool, err error)
	// Has reports whether any element matches xpath.
	Has(ctx context.Context, xpath string) (bool, error)
	// Cookies returns name->value cookies visible to the current tab.
	Cookies(ctx context.Context) (map[string]string, error)
	// Close shuts the browser down.
	Close() error
}

// ZaiFarmState is the observable harvest state, mirroring zai_farm_state.
// Status is one of: idle, running, error, completed.
type ZaiFarmState struct {
	Status            string
	Progress          int
	CurrentAccount    int
	TotalAccounts     int
	HarvestedCount    int
	HarvestedAccounts []ZaiHarvestedAccount
	Logs              []string
	CurrentStep       string
	LastError         string
}

// ZaiHarvestedAccount is one farmed account, mirroring the account_data dict.
type ZaiHarvestedAccount struct {
	Name         string
	Email        string
	Password     string
	AccessToken  string
	RefreshToken string
	CreatedAt    time.Time
}

// ZaiAccountStore persists harvested accounts, replacing save_to_autoclawpi_db.
// A store failure is logged as a warning and does not fail the harvest, exactly
// like the Python try/except around each db_path attempt.
//
// Production wiring: implement on top of modernc.org/sqlite using
// ZaiAutoClawDBPaths for the candidate locations, CREATE TABLE IF NOT EXISTS
// accounts (...), device_id "autoclawpi-<epoch-ms>", points 10540, active 1.
type ZaiAccountStore interface {
	SaveAccount(ctx context.Context, a ZaiHarvestedAccount) error
}

// ZaiAccountFileSink appends "email:password:access_token" lines to the harvest
// file, mirroring the output/zai_accounts.txt append in run_zai_farm. An empty
// Path falls back to DefaultZaiAccountsPath.
type ZaiAccountFileSink struct {
	Path string
}

// Append appends one "email:password:access_token" line, creating parent dirs.
func (s ZaiAccountFileSink) Append(a ZaiHarvestedAccount) error {
	path := s.Path
	if path == "" {
		path = DefaultZaiAccountsPath
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("petaniproxy: ZaiAccountFileSink mkdir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("petaniproxy: ZaiAccountFileSink open: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s:%s:%s\n", a.Email, a.Password, a.AccessToken); err != nil {
		return fmt.Errorf("petaniproxy: ZaiAccountFileSink write: %w", err)
	}
	return nil
}

// ZaiFarm is the account-farming orchestrator, mirroring run_zai_farm plus the
// module-level zai_farm_state. Zero value is not usable: build with NewZaiFarm
// or set Driver plus the optional seams before calling Run.
type ZaiFarm struct {
	// Driver performs browser automation; nil fails the run (like Chromium()
	// raising in Python). The caller constructs it, applying headless there.
	Driver ZaiBrowserDriver
	// Store persists accounts; nil disables DB persistence.
	Store ZaiAccountStore
	// Sink receives the email:password:token lines.
	Sink ZaiAccountFileSink
	// NewMail builds a fresh disposable-mailbox service per account.
	NewMail func() *ZaiDuckMailService

	// LogHook, when set, receives every log line in addition to the ring buffer.
	LogHook func(level, message string)
	// Out receives the default stdout-style log lines; defaults to os.Stdout.
	Out io.Writer
	// Now supplies timestamps; defaults to time.Now.
	Now func() time.Time
	// Rand supplies randomness; defaults to a time-seeded source.
	Rand *rand.Rand
	// Sleep pauses between automation steps; defaults to time.Sleep.
	// Override with a no-op in tests.
	Sleep func(time.Duration)
	// OTPTimeout bounds WaitForOTP per account; defaults to 50s like Python.
	OTPTimeout time.Duration

	mu    sync.Mutex
	state ZaiFarmState
}

// NewZaiFarm builds a ZaiFarm with sane defaults; the caller may still tweak
// the exported fields (Rand, Sleep, OTPTimeout, ...) afterwards.
func NewZaiFarm(driver ZaiBrowserDriver, store ZaiAccountStore, sink ZaiAccountFileSink) *ZaiFarm {
	return &ZaiFarm{
		Driver:     driver,
		Store:      store,
		Sink:       sink,
		NewMail:    NewZaiDuckMailService,
		Out:        os.Stdout,
		Now:        time.Now,
		Rand:       rand.New(rand.NewSource(time.Now().UnixNano())),
		Sleep:      time.Sleep,
		OTPTimeout: 50 * time.Second,
		state:      ZaiFarmState{Status: "idle", CurrentStep: "idle"},
	}
}

// Snapshot returns a deep copy of the current farm state.
func (f *ZaiFarm) Snapshot() ZaiFarmState {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := f.state
	cp.HarvestedAccounts = append([]ZaiHarvestedAccount(nil), f.state.HarvestedAccounts...)
	cp.Logs = append([]string(nil), f.state.Logs...)
	return cp
}

// log mirrors zai_log(): timestamped line, level prefix, ring-buffer cap,
// optional step update, optional external hook. Called with f.mu held by
// callers or taking it here; it takes the lock itself.
func (f *ZaiFarm) log(level, message, step string) {
	now := f.Now().Format("15:04:05")
	line := fmt.Sprintf("[%s] %s", now, message)

	prefix := "[*]"
	switch level {
	case ZaiLogError:
		prefix = "[!]"
	case ZaiLogSuccess:
		prefix = "[+]"
	case ZaiLogDebug:
		prefix = "[~]"
	}
	out := f.Out
	if out == nil {
		out = io.Discard
	}
	fmt.Fprintf(out, "%s %s\n", prefix, line)

	f.mu.Lock()
	if step != "" {
		f.state.CurrentStep = step
	}
	f.state.Logs = append(f.state.Logs, line)
	if len(f.state.Logs) > zaiMaxLogEntries {
		f.state.Logs = f.state.Logs[len(f.state.Logs)-zaiMaxLogEntries:]
	}
	hook := f.LogHook
	f.mu.Unlock()

	if hook != nil {
		hook(level, message)
	}
}

// setLocked helpers assume the caller holds f.mu.
func (f *ZaiFarm) setStatusLocked(status, step, lastErr string) {
	f.state.Status = status
	f.state.CurrentStep = step
	f.state.LastError = lastErr
}

// Run farms total accounts, mirroring run_zai_farm(total, headless).
// headless is kept for API parity: the injected Driver owns browser
// construction, so the caller applies headless when building the driver
// (e.g. the chromedp headless flag).
func (f *ZaiFarm) Run(ctx context.Context, total int, headless bool) ([]ZaiHarvestedAccount, error) {
	f.mu.Lock()
	f.state = ZaiFarmState{
		Status:        "running",
		TotalAccounts: total,
		CurrentStep:   "opening_browser",
	}
	f.mu.Unlock()

	f.log(ZaiLogInfo,
		fmt.Sprintf("Memulai Mesin Ternak Z.ai / AutoClaw (Target: %d akun, Headless: %v)", total, headless), "")

	if f.Driver == nil {
		err := errors.New("petaniproxy: ZaiFarm.Driver is nil; wire a ZaiBrowserDriver before Run")
		f.log(ZaiLogError, fmt.Sprintf("Gagal menginisialisasi Chromium: %v", err), "")
		f.mu.Lock()
		f.setStatusLocked("error", f.state.CurrentStep, err.Error())
		f.mu.Unlock()
		return nil, err
	}

	harvested := make([]ZaiHarvestedAccount, 0, total)
	for idx := 1; idx <= total; idx++ {
		if err := ctx.Err(); err != nil {
			f.mu.Lock()
			f.setStatusLocked("error", f.state.CurrentStep, err.Error())
			f.mu.Unlock()
			return harvested, err
		}
		acct, err := f.harvestOne(ctx, idx, total)
		if err != nil {
			f.log(ZaiLogError, fmt.Sprintf("Akun [%d/%d] Gagal: %v", idx, total, err), "")
			continue
		}
		harvested = append(harvested, acct)
		f.Sleep(2 * time.Second)
	}

	if err := f.Driver.Close(); err != nil {
		f.log(ZaiLogWarning, fmt.Sprintf("browser quit: %v", err), "")
	}

	f.mu.Lock()
	f.setStatusLocked("completed", "completed", "")
	f.state.Progress = 100
	f.mu.Unlock()

	f.log(ZaiLogSuccess,
		fmt.Sprintf("Selesai! Berhasil memanen %d/%d akun Z.ai.", len(harvested), total), "")
	return harvested, nil
}

// harvestOne runs the per-account signup flow for account idx of total.
func (f *ZaiFarm) harvestOne(ctx context.Context, idx, total int) (ZaiHarvestedAccount, error) {
	var zero ZaiHarvestedAccount

	f.mu.Lock()
	f.state.CurrentAccount = idx
	f.state.Progress = int(float64(idx-1) / float64(total) * 100)
	f.mu.Unlock()

	f.log(ZaiLogInfo, fmt.Sprintf("--- Memproses Akun [%d/%d] ---", idx, total), "opening_browser")

	newMail := f.NewMail
	if newMail == nil {
		newMail = NewZaiDuckMailService
	}
	mail := newMail()
	email, password, err := mail.CreateMailbox(ctx)
	if err != nil {
		return zero, fmt.Errorf("create mailbox: %w", err)
	}
	fullName := ZaiDisplayName(f.Rand)
	f.log(ZaiLogSuccess, fmt.Sprintf("Mailbox Siap: %s | Password: %s", email, password), "")

	d := f.Driver
	f.log(ZaiLogInfo, "Membuka halaman registrasi Z.ai: "+ZaiAuthURL, "entering_email")
	if err := d.Navigate(ctx, ZaiAuthURL); err != nil {
		return zero, fmt.Errorf("navigate: %w", err)
	}
	f.Sleep(3 * time.Second)

	if err := f.fillIfPresent(ctx, zaiXPathNameInput, fullName); err != nil {
		return zero, err
	}
	if err := f.fillIfPresent(ctx, zaiXPathEmailInput, email); err != nil {
		return zero, err
	}
	if err := f.fillIfPresent(ctx, zaiXPathPassInput, password); err != nil {
		return zero, err
	}

	if found, err := d.Click(ctx, zaiXPathCaptchaBtn); err != nil {
		return zero, fmt.Errorf("click captcha: %w", err)
	} else if found {
		f.log(ZaiLogWarning, "Mengklik verifikasi Captcha...", "")
		f.Sleep(2 * time.Second)
	}

	if found, err := d.Click(ctx, zaiXPathSubmitBtn); err != nil {
		return zero, fmt.Errorf("click submit: %w", err)
	} else if found {
		f.log(ZaiLogInfo, "Mengklik tombol 'Create Account'...", "submitting")
		f.Sleep(5 * time.Second)
	}

	if err := f.maybeVerifyOTP(ctx, mail); err != nil {
		return zero, err
	}

	cookies, err := d.Cookies(ctx)
	if err != nil {
		return zero, fmt.Errorf("read cookies: %w", err)
	}
	accessToken, refreshToken := ZaiSelectTokens(cookies, f.Rand)

	acct := ZaiHarvestedAccount{
		Name:         fullName,
		Email:        email,
		Password:     password,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		CreatedAt:    f.Now(),
	}

	if f.Store != nil {
		if err := f.Store.SaveAccount(ctx, acct); err != nil {
			f.log(ZaiLogWarning, fmt.Sprintf("Gagal simpan ke DB: %v", err), "")
		}
	}
	if err := f.Sink.Append(acct); err != nil {
		return zero, err
	}

	f.mu.Lock()
	f.state.HarvestedCount++
	f.state.HarvestedAccounts = append(f.state.HarvestedAccounts, acct)
	f.mu.Unlock()

	f.log(ZaiLogSuccess, fmt.Sprintf("Akun [%d/%d] BERHASIL DITERNAK: %s", idx, total, email), "")
	return acct, nil
}

// fillIfPresent fills the field when the xpath matches, sleeping 0.5s after a
// successful fill like the Python time.sleep(0.5) calls.
func (f *ZaiFarm) fillIfPresent(ctx context.Context, xpath, value string) error {
	found, err := f.Driver.Fill(ctx, xpath, value)
	if err != nil {
		return fmt.Errorf("fill %s: %w", xpath, err)
	}
	if found {
		f.Sleep(500 * time.Millisecond)
	}
	return nil
}

// maybeVerifyOTP handles the optional 6-digit OTP step: only when the OTP
// input is present, wait for the code, fill it, and click Verify/Confirm.
func (f *ZaiFarm) maybeVerifyOTP(ctx context.Context, mail *ZaiDuckMailService) error {
	found, err := f.Driver.Has(ctx, zaiXPathOTPInput)
	if err != nil {
		return fmt.Errorf("probe otp input: %w", err)
	}
	if !found {
		return nil
	}
	f.log(ZaiLogWarning, "Menunggu kode OTP verifikasi 6 digit...", "waiting_otp")
	otp, err := mail.WaitForOTP(ctx, f.otpTimeout())
	if err != nil {
		return fmt.Errorf("wait for otp: %w", err)
	}
	if otp == "" {
		return nil
	}
	f.log(ZaiLogSuccess, "OTP Diterima: "+otp, "verifying_otp")
	if _, err := f.Driver.Fill(ctx, zaiXPathOTPInput, otp); err != nil {
		return fmt.Errorf("fill otp: %w", err)
	}
	f.Sleep(time.Second)
	if _, err := f.Driver.Click(ctx, zaiXPathOTPConfirm); err != nil {
		return fmt.Errorf("click otp confirm: %w", err)
	}
	f.Sleep(4 * time.Second)
	return nil
}

func (f *ZaiFarm) otpTimeout() time.Duration {
	if f.OTPTimeout > 0 {
		return f.OTPTimeout
	}
	return 50 * time.Second
}
