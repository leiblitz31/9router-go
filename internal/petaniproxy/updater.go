// updater.go - Go port of petani-proxy core/updater.py.
//
// Auto-updater & version patch notification system: checks a remote
// version.json on GitHub (plus git commit distance as fallback), renders
// terminal banners/announcements, and performs the update (git pull +
// auto_run_commands + optional self-restart).
//
// Behavioral parity notes (Python -> Go):
//   - get_base_dir() used __file__; Go has no equivalent, so NewUpdater("")
//     falls back to os.Getwd() and callers should pass an explicit base dir.
//   - Python's requests timeout=2.5 for the GET maps to Updater.CheckTimeout
//     (default DefaultUpdateCheckTimeout) applied as a context deadline.
//   - check_for_updates never fails hard: transport problems land in
//     UpdateCheckResult.Error, mirroring Python's "error" dict key.
//   - perform_update normalizes "python ..."/"pip ..." prefixes against
//     sys.executable; Go normalizes against Updater.PythonBin.
//   - show_full_announcement printed to stdout; Go exposes
//     FormatFullAnnouncement returning the string (caller prints it).
//   - os.execv self-restart maps to syscall.Exec via restartProcessFunc,
//     a package var so tests can stub it.
package petaniproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultGitHubRepo mirrors GITHUB_REPO in updater.py.
const DefaultGitHubRepo = "itzluthfi/petani-proxy"

// DefaultRemoteVersionURL mirrors REMOTE_VERSION_URL in updater.py.
const DefaultRemoteVersionURL = "https://raw.githubusercontent.com/" + DefaultGitHubRepo + "/master/version.json"

// DefaultUpdateCheckTimeout bounds the remote version.json fetch.
// (Python used timeout=2.5 for the GET and 2-3s for the git probes;
// the git probes keep their own 2-3s deadlines below.)
const DefaultUpdateCheckTimeout = 5 * time.Second

// DefaultGitBin and DefaultPythonBin are the binaries invoked by the updater.
const DefaultGitBin = "git"
const DefaultPythonBin = "python3"

// ANSI colors mirroring colorama's Fore/Style used by updater.py.
const (
	cYellow  = "\x1b[33m"
	cWhite   = "\x1b[37m"
	cBright  = "\x1b[1m"
	cReset   = "\x1b[0m"
	cCyan    = "\x1b[36m"
	cDim     = "\x1b[90m"
	cMagenta = "\x1b[35m"
	cGreen   = "\x1b[32m"
	cRed     = "\x1b[31m"
)

// VersionInfo mirrors the version.json metadata read by get_local_version_info.
type VersionInfo struct {
	Version         string   `json:"version"`
	ReleaseDate     string   `json:"release_date"`
	Title           string   `json:"title"`
	Announcement    string   `json:"announcement"`
	Changelog       []string `json:"changelog"`
	AutoRunCommands []string `json:"auto_run_commands"`
	ActionsRequired []string `json:"actions_required"`
}

// defaultVersionInfo mirrors the fallback dict in get_local_version_info.
func defaultVersionInfo() VersionInfo {
	return VersionInfo{
		Version:      "1.0.0",
		ReleaseDate:  "2026-09-14",
		Title:        "🌾 PetaniProxy v1.0.0",
		Announcement: "Rilis perdana PetaniProxy v1.0",
		Changelog:    []string{},
	}
}

// LoadLocalVersionInfo reads <baseDir>/version.json, mirroring
// get_local_version_info. Any failure (missing/unparseable file) yields the
// defaults; present keys overlay the defaults like dict.get at use sites.
func LoadLocalVersionInfo(baseDir string) VersionInfo {
	info := defaultVersionInfo()
	data, err := os.ReadFile(filepath.Join(baseDir, "version.json"))
	if err != nil {
		return info
	}
	var raw VersionInfo
	if err := json.Unmarshal(data, &raw); err != nil {
		return info
	}
	if raw.Version != "" {
		info.Version = raw.Version
	}
	if raw.ReleaseDate != "" {
		info.ReleaseDate = raw.ReleaseDate
	}
	if raw.Title != "" {
		info.Title = raw.Title
	}
	if raw.Announcement != "" {
		info.Announcement = raw.Announcement
	}
	if raw.Changelog != nil {
		info.Changelog = raw.Changelog
	}
	if raw.AutoRunCommands != nil {
		info.AutoRunCommands = raw.AutoRunCommands
	}
	if raw.ActionsRequired != nil {
		info.ActionsRequired = raw.ActionsRequired
	}
	return info
}

// ParseVersionTuple parses "1.0.0" or "v1.2.3" into a comparable int slice,
// padding to at least 3 components; non-numeric components become 0.
// Mirrors parse_version_tuple.
func ParseVersionTuple(v string) []int {
	clean := strings.TrimLeft(strings.ToLower(strings.TrimSpace(v)), "v")
	var parts []int
	for _, p := range strings.Split(clean, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			n = 0
		}
		parts = append(parts, n)
	}
	for len(parts) < 3 {
		parts = append(parts, 0)
	}
	return parts
}

// CompareVersionTuples compares two version tuples with zero-padding,
// mirroring Python tuple comparison: -1 if a<b, 0 if equal, 1 if a>b.
func CompareVersionTuples(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

// UpdateCheckResult mirrors the dict returned by check_for_updates.
type UpdateCheckResult struct {
	HasUpdate       bool
	CurrentVersion  string
	RemoteVersion   string
	Title           string
	Announcement    string
	ReleaseDate     string
	Changelog       []string
	AutoRunCommands []string
	ActionsRequired []string
	// Error records a transport failure, mirroring Python's "error" key.
	Error string
}

// CommandRunner executes external processes. It is the seam that makes
// CheckForUpdates and PerformUpdate testable without git or a shell.
type CommandRunner interface {
	// Run executes name with args in dir and returns stdout.
	Run(ctx context.Context, dir, name string, args ...string) (string, error)
	// RunShell executes a shell command line in dir (mirrors shell=True).
	RunShell(ctx context.Context, dir, command string) error
}

// execCommandRunner is the production CommandRunner backed by os/exec.
type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	// Stderr left nil so the ExitError.Stderr field still captures output.
	if err := cmd.Run(); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

func (execCommandRunner) RunShell(ctx context.Context, dir, command string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	return cmd.Run()
}

// Updater bundles the configuration for version checks and self-updates,
// mirroring the module-level functions of updater.py.
type Updater struct {
	// BaseDir is the petani-proxy root (contains version.json / .git).
	BaseDir string
	// RemoteVersionURL overrides DefaultRemoteVersionURL (seam for tests).
	RemoteVersionURL string
	// CheckTimeout bounds the remote version.json fetch.
	CheckTimeout time.Duration
	// Client performs the version.json fetch; nil means http.DefaultClient.
	Client *http.Client
	// Runner executes git/shell commands; nil means execCommandRunner.
	Runner CommandRunner
	// GitBin is the git binary name (DefaultGitBin).
	GitBin string
	// PythonBin replaces a "python "/"pip " prefix in auto_run_commands
	// (mirrors sys.executable normalization in perform_update).
	PythonBin string
}

// NewUpdater builds an Updater rooted at baseDir; an empty baseDir falls
// back to the current working directory (Go has no __file__ equivalent).
func NewUpdater(baseDir string) *Updater {
	if baseDir == "" {
		if wd, err := os.Getwd(); err == nil {
			baseDir = wd
		}
	}
	return &Updater{
		BaseDir:          baseDir,
		RemoteVersionURL: DefaultRemoteVersionURL,
		CheckTimeout:     DefaultUpdateCheckTimeout,
		Runner:           execCommandRunner{},
		GitBin:           DefaultGitBin,
		PythonBin:        DefaultPythonBin,
	}
}

func (u *Updater) runner() CommandRunner {
	if u.Runner != nil {
		return u.Runner
	}
	return execCommandRunner{}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// fetchRemoteVersion GETs the remote version.json; a non-200 response yields
// (nil, nil) so the caller falls through to the git check, like Python.
func (u *Updater) fetchRemoteVersion(ctx context.Context) (*VersionInfo, error) {
	client := u.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.RemoteVersionURL, nil)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy.Updater.fetchRemoteVersion: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("petaniproxy.Updater.fetchRemoteVersion: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var info VersionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("petaniproxy.Updater.fetchRemoteVersion: %w", err)
	}
	return &info, nil
}

// gitAheadCount mirrors the git commit-distance fallback in check_for_updates:
// fetch origin/master, then count HEAD..origin/master and grab up to 5
// commit subjects. Returns 0 when not a git checkout or any step fails.
func (u *Updater) gitAheadCount(ctx context.Context) (int, []string) {
	if _, err := os.Stat(filepath.Join(u.BaseDir, ".git")); err != nil {
		return 0, nil
	}
	r := u.runner()
	fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := r.Run(fetchCtx, u.BaseDir, u.GitBin, "fetch", "origin", "master", "--quiet"); err != nil {
		return 0, nil
	}
	revCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := r.Run(revCtx, u.BaseDir, u.GitBin, "rev-list", "--count", "HEAD..origin/master")
	if err != nil {
		return 0, nil
	}
	ahead, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || ahead <= 0 {
		return 0, nil
	}
	logCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var changelog []string
	if logOut, err := r.Run(logCtx, u.BaseDir, u.GitBin, "log", "-n", strconv.Itoa(min(ahead, 5)), "--pretty=format:%s", "origin/master"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(logOut), "\n") {
			if s := strings.TrimSpace(line); s != "" {
				changelog = append(changelog, s)
			}
		}
	}
	return ahead, changelog
}

// CheckForUpdates mirrors check_for_updates: compare the remote version.json
// against the local one (early return when the remote is newer), then fall
// back to git commit distance. Transport failures are recorded in
// UpdateCheckResult.Error instead of failing, like Python's "error" key.
func (u *Updater) CheckForUpdates(ctx context.Context) UpdateCheckResult {
	local := LoadLocalVersionInfo(u.BaseDir)
	result := UpdateCheckResult{
		CurrentVersion:  local.Version,
		RemoteVersion:   local.Version,
		Title:           local.Title,
		Announcement:    local.Announcement,
		ReleaseDate:     local.ReleaseDate,
		Changelog:       local.Changelog,
		AutoRunCommands: local.AutoRunCommands,
		ActionsRequired: local.ActionsRequired,
	}

	timeout := u.CheckTimeout
	if timeout <= 0 {
		timeout = DefaultUpdateCheckTimeout
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	remote, err := u.fetchRemoteVersion(fetchCtx)
	cancel()
	if err != nil {
		result.Error = err.Error()
	} else if remote != nil {
		remoteVer := firstNonEmpty(remote.Version, result.RemoteVersion)
		result.RemoteVersion = remoteVer
		if remote.Title != "" {
			result.Title = remote.Title
		} else {
			result.Title = "PetaniProxy v" + remoteVer
		}
		result.Announcement = remote.Announcement
		result.ReleaseDate = firstNonEmpty(remote.ReleaseDate, "-")
		result.Changelog = remote.Changelog
		result.AutoRunCommands = remote.AutoRunCommands
		result.ActionsRequired = remote.ActionsRequired

		if CompareVersionTuples(ParseVersionTuple(remoteVer), ParseVersionTuple(result.CurrentVersion)) > 0 {
			result.HasUpdate = true
			return result
		}
	}

	if ahead, logLines := u.gitAheadCount(ctx); ahead > 0 {
		result.HasUpdate = true
		if len(result.Changelog) == 0 {
			result.Changelog = logLines
		}
	}
	return result
}

// RenderUpdateBanner mirrors render_update_banner: a colored terminal banner
// announcing an available update. lang "ID" selects Indonesian, anything
// else English.
func RenderUpdateBanner(info UpdateCheckResult, lang string) string {
	curr := firstNonEmpty(info.CurrentVersion, "1.0.0")
	remote := firstNonEmpty(info.RemoteVersion, "1.0.0")
	title := info.Title
	_ = title // title is part of the result but the banner keys off versions
	announcement := info.Announcement
	changelog := info.Changelog
	id := lang == "ID"

	var sb strings.Builder
	bar := strings.Repeat("━", 76)
	thin := strings.Repeat("─", 76)
	sb.WriteString(cYellow + bar + cReset + "\n")
	if id {
		fmt.Fprintf(&sb, "  %s%s🚀 UPDATE TERSEDIA! %sv%s%s (Versi Terpasang: %sv%s%s)%s\n",
			cWhite, cBright, cGreen, remote, cWhite, cYellow, curr, cWhite, cReset)
	} else {
		fmt.Fprintf(&sb, "  %s%s🚀 NEW UPDATE AVAILABLE! %sv%s%s (Current: %sv%s%s)%s\n",
			cWhite, cBright, cGreen, remote, cWhite, cYellow, curr, cWhite, cReset)
	}
	if announcement != "" {
		fmt.Fprintf(&sb, "  📢 %s%s%s\n", cCyan, announcement, cReset)
	}
	sb.WriteString(cDim + thin + cReset + "\n")
	if id {
		fmt.Fprintf(&sb, "  %s📝 Rincian Pembaruan & Fitur Baru:%s\n", cMagenta, cReset)
	} else {
		fmt.Fprintf(&sb, "  %s📝 Patch Notes & What's New:%s\n", cMagenta, cReset)
	}
	if len(changelog) > 0 {
		for _, item := range changelog[:min(len(changelog), 4)] {
			fmt.Fprintf(&sb, "   %s•%s %s%s\n", cYellow, cWhite, strings.TrimSpace(item), cReset)
		}
	} else {
		hint := "Performance improvements & bug fixes."
		if id {
			hint = "Peningkatan performa & perbaikan bug."
		}
		fmt.Fprintf(&sb, "   %s•%s %s%s\n", cYellow, cWhite, hint, cReset)
	}
	sb.WriteString(cDim + thin + cReset + "\n")
	cta := "👉 Press [U] in menu for seamless 1-click update! ✨"
	if id {
		cta = "👉 Tekan [U] di menu untuk update otomatis dalam 1 klik! ✨"
	}
	fmt.Fprintf(&sb, "  %s%s%s%s\n", cGreen, cBright, cta, cReset)
	sb.WriteString(cYellow + bar + cReset)
	return sb.String()
}

// FormatFullAnnouncement mirrors show_full_announcement but returns the text
// instead of printing it, so callers (and tests) control the output sink.
func FormatFullAnnouncement(info VersionInfo, lang string) string {
	ver := firstNonEmpty(info.Version, "1.0.0")
	date := firstNonEmpty(info.ReleaseDate, "-")
	title := firstNonEmpty(info.Title, "PetaniProxy v"+ver)
	ann := info.Announcement
	changelog := info.Changelog
	actions := info.ActionsRequired

	var sb strings.Builder
	bar := strings.Repeat("━", 76)
	fmt.Fprintf(&sb, "\n%s%s%s\n", cCyan, bar, cReset)
	fmt.Fprintf(&sb, "%s%s📢 INFORMASI RILIS & PENGUMUMAN UPDATE (PETANIPROXY v%s)%s\n", cWhite, cBright, ver, cReset)
	fmt.Fprintf(&sb, "  • Tanggal Rilis : %s%s%s\n", cYellow, date, cReset)
	fmt.Fprintf(&sb, "  • Judul Rilis   : %s%s%s\n", cGreen, title, cReset)
	if ann != "" {
		fmt.Fprintf(&sb, "  • Highlight     : %s%s%s\n", cCyan, ann, cReset)
	}
	if len(changelog) > 0 {
		fmt.Fprintf(&sb, "\n%sDAFTAR LENGKAP FITUR BARU & PATCH NOTES:%s\n", cMagenta, cReset)
		for idx, item := range changelog {
			fmt.Fprintf(&sb, "  %s%2d.%s %s%s\n", cYellow, idx+1, cWhite, item, cReset)
		}
	}
	if len(actions) > 0 {
		fmt.Fprintf(&sb, "\n%s⚠️  CATATAN KHUSUS / TINDAKAN DIPERLUKAN:%s\n", cYellow, cReset)
		for _, a := range actions {
			fmt.Fprintf(&sb, "  %s✓%s %s%s\n", cGreen, cWhite, a, cReset)
		}
	}
	fmt.Fprintf(&sb, "%s%s%s\n", cCyan, bar, cReset)
	return sb.String()
}

// restartProcessFunc re-execs the current process, mirroring os.execv in
// perform_update. It is a package var so tests can stub it.
var restartProcessFunc = func() error {
	path, err := exec.LookPath(os.Args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, os.Args, os.Environ())
}

// UpdateOptions configures PerformUpdate.
type UpdateOptions struct {
	// Restart prompts for restart after updating (mirrors the [Y/n] prompt).
	Restart bool
	// Lang is "ID" (default) or anything else for English.
	Lang string
	// Stdout receives progress output; defaults to os.Stdout.
	Stdout io.Writer
	// Stdin supplies the restart answer; defaults to os.Stdin when Restart.
	Stdin io.Reader
}

// normalizeCommand maps a "python ..."/"pip ..." prefix to the configured
// PythonBin, mirroring the sys.executable normalization in perform_update.
func (u *Updater) normalizeCommand(cmd string) string {
	if strings.HasPrefix(cmd, "python ") {
		return strconv.Quote(u.PythonBin) + cmd[len("python"):]
	}
	if strings.HasPrefix(cmd, "pip ") {
		return strconv.Quote(u.PythonBin) + " -m pip" + cmd[len("pip"):]
	}
	return cmd
}

// PerformUpdate mirrors perform_update: git pull (fetch + hard-reset
// fallback), runs auto_run_commands from the new version.json, prints the
// announcement, and optionally prompts for a self-restart. Per-command
// shell failures are ignored like Python's unchecked subprocess.run.
func (u *Updater) PerformUpdate(ctx context.Context, opts UpdateOptions) error {
	lang := opts.Lang
	if lang == "" {
		lang = "ID"
	}
	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	id := lang == "ID"
	tr := func(idStr, enStr string) string {
		if id {
			return idStr
		}
		return enStr
	}
	pf := func(format string, args ...any) {
		fmt.Fprintf(out, format+"\n", args...)
	}
	bar := strings.Repeat("━", 76)

	pf("")
	pf("%s%s%s", cCyan, bar, cReset)
	pf("%s%s%s%s", cGreen, cBright,
		tr("🚀 MEMULAI PROSES UPDATE PETANIPROXY...", "🚀 LAUNCHING PETANIPROXY AUTO-UPDATE..."), cReset)
	pf("%s%s%s", cDim,
		tr("Menghubungkan ke repositori pusat (github.com/"+DefaultGitHubRepo+")...",
			"Connecting to upstream repo (github.com/"+DefaultGitHubRepo+")..."), cReset)
	pf("")

	// Step 1: git pull, with fetch + hard-reset fallback on non-zero exit.
	pf("  %s[1/3]%s %s", cYellow, cWhite,
		tr("Mengambil commit & patch terbaru via Git...", "Pulling latest commits & patches via Git..."))
	r := u.runner()
	gitOut, err := r.Run(ctx, u.BaseDir, u.GitBin, "pull", "origin", "master")
	if err == nil {
		first := "Up to date"
		if lines := strings.Split(strings.TrimSpace(gitOut), "\n"); len(lines) > 0 && lines[0] != "" {
			first = lines[0]
		}
		pf("       %s✓ Git Pull Sukses:%s %s", cGreen, cReset, first)
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			// The git binary itself failed to run (mirrors Python's False).
			pf("       %s❌ Gagal menjalankan git: %v%s", cRed, err, cReset)
			return fmt.Errorf("petaniproxy.Updater.PerformUpdate: git pull: %w", err)
		}
		pf("       %s⚠️ Git Pull Gagal:%s %s", cRed, cReset, strings.TrimSpace(string(exitErr.Stderr)))
		pf("       %s%s%s", cYellow,
			tr("Mencoba fetch & reset hard ke origin/master...", "Trying fetch & hard reset to origin/master..."), cReset)
		_, _ = r.Run(ctx, u.BaseDir, u.GitBin, "fetch", "origin", "master")
		_, _ = r.Run(ctx, u.BaseDir, u.GitBin, "reset", "--hard", "origin/master")
	}

	// Step 2: run auto_run_commands from the updated version.json.
	newInfo := LoadLocalVersionInfo(u.BaseDir)
	autoCmds := newInfo.AutoRunCommands
	if len(autoCmds) == 0 {
		autoCmds = []string{"python -m pip install -r requirements.txt"}
	}
	pf("")
	pf("  %s[2/3]%s %s", cYellow, cWhite,
		tr("Menjalankan instruksi pembaruan versi baru...", "Executing version update commands..."))
	for _, cmd := range autoCmds {
		pf("       %s⚙️ Menjalankan:%s %s", cCyan, cReset, cmd)
		_ = r.RunShell(ctx, u.BaseDir, u.normalizeCommand(cmd))
	}
	pf("       %s✓ Semua instruksi pembaruan sukses diselesaikan!%s", cGreen, cReset)

	// Step 3: announcement + success banner.
	pf("")
	pf("  %s[3/3]%s %s", cYellow, cWhite,
		tr("Membaca pengumuman & rincian versi baru...", "Reading updated version announcement..."))
	fmt.Fprintln(out, FormatFullAnnouncement(newInfo, lang))
	pf("%s%s%s%s", cGreen, cBright,
		tr("🎉 PETANIPROXY SUKSES DIPERBARUI KE v"+newInfo.Version+"!",
			"🎉 PETANIPROXY SUCCESSFULLY UPDATED TO v"+newInfo.Version+"!"), cReset)
	pf("%sSemua fitur dan patch baru siap digunakan! 🌾🚜%s", cWhite, cReset)
	pf("")

	if opts.Restart {
		fmt.Fprint(out, cYellow+tr("Ingin me-restart PetaniProxy sekarang? [Y/n]: ",
			"Restart PetaniProxy now? [Y/n]: ")+cReset)
		line, rerr := bufio.NewReader(stdin).ReadString('\n')
		// Mirror Python's try/except around input(): a failed read skips restart.
		if rerr == nil || len(line) > 0 {
			answer := strings.ToLower(strings.TrimSpace(line))
			if answer == "" || answer == "y" || answer == "yes" {
				pf("%s%s%s", cGreen, tr("Memuat ulang program...", "Restarting program..."), cReset)
				pf("")
				if rerr := restartProcessFunc(); rerr != nil {
					return fmt.Errorf("petaniproxy.Updater.PerformUpdate: restart: %w", rerr)
				}
			}
		}
	}
	return nil
}
