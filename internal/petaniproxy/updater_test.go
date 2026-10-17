package petaniproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubRunner is a fake CommandRunner recording invocations.
type stubRunner struct {
	runFunc    func(ctx context.Context, dir, name string, args ...string) (string, error)
	shellFunc  func(ctx context.Context, dir, command string) error
	runCalls   []string
	shellCalls []string
}

func (s *stubRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	s.runCalls = append(s.runCalls, name+" "+strings.Join(args, " "))
	if s.runFunc != nil {
		return s.runFunc(ctx, dir, name, args...)
	}
	return "", nil
}

func (s *stubRunner) RunShell(ctx context.Context, dir, command string) error {
	s.shellCalls = append(s.shellCalls, command)
	if s.shellFunc != nil {
		return s.shellFunc(ctx, dir, command)
	}
	return nil
}

func TestParseVersionTuple(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []int
	}{
		{"plain", "1.0.0", []int{1, 0, 0}},
		{"v prefix", "v1.2.3", []int{1, 2, 3}},
		{"short pads to three", "1.2", []int{1, 2, 0}},
		{"longer kept", "1.2.3.4", []int{1, 2, 3, 4}},
		{"non-numeric becomes zero", "1.x.3", []int{1, 0, 3}},
		{"garbage", "abc", []int{0, 0, 0}},
		{"whitespace and case", "  V2.0  ", []int{2, 0, 0}},
		{"empty", "", []int{0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseVersionTuple(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseVersionTuple(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ParseVersionTuple(%q) = %v, want %v", tt.input, got, tt.want)
				}
			}
		})
	}
}

func TestCompareVersionTuples(t *testing.T) {
	tests := []struct {
		name string
		a, b []int
		want int
	}{
		{"less", []int{1, 0, 0}, []int{1, 0, 1}, -1},
		{"greater", []int{2, 0}, []int{1, 9, 9}, 1},
		{"equal different lengths", []int{1, 2}, []int{1, 2, 0}, 0},
		{"equal", []int{1, 2, 3}, []int{1, 2, 3}, 0},
		{"longer wins", []int{1, 2, 3, 1}, []int{1, 2, 3}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompareVersionTuples(tt.a, tt.b); got != tt.want {
				t.Errorf("CompareVersionTuples(%v, %v) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestLoadLocalVersionInfo(t *testing.T) {
	t.Run("missing file yields defaults", func(t *testing.T) {
		info := LoadLocalVersionInfo(t.TempDir())
		if info.Version != "1.0.0" {
			t.Errorf("Version = %q, want 1.0.0", info.Version)
		}
		if !strings.Contains(info.Title, "PetaniProxy") {
			t.Errorf("Title = %q, want PetaniProxy default", info.Title)
		}
	})

	t.Run("valid file parsed", func(t *testing.T) {
		dir := t.TempDir()
		payload := `{"version":"2.1.0","release_date":"2026-10-01","title":"t","announcement":"a","changelog":["x","y"],"auto_run_commands":["echo hi"],"actions_required":["do z"]}`
		if err := os.WriteFile(filepath.Join(dir, "version.json"), []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
		info := LoadLocalVersionInfo(dir)
		if info.Version != "2.1.0" || info.ReleaseDate != "2026-10-01" {
			t.Errorf("unexpected info: %+v", info)
		}
		if len(info.Changelog) != 2 || len(info.AutoRunCommands) != 1 || len(info.ActionsRequired) != 1 {
			t.Errorf("slices not parsed: %+v", info)
		}
	})

	t.Run("malformed file yields defaults", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "version.json"), []byte("{nope"), 0o644); err != nil {
			t.Fatal(err)
		}
		if info := LoadLocalVersionInfo(dir); info.Version != "1.0.0" {
			t.Errorf("Version = %q, want default 1.0.0", info.Version)
		}
	})

	t.Run("partial file keeps defaults for missing keys", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "version.json"), []byte(`{"version":"3.0.0"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		info := LoadLocalVersionInfo(dir)
		if info.Version != "3.0.0" {
			t.Errorf("Version = %q", info.Version)
		}
		if !strings.Contains(info.Title, "PetaniProxy") {
			t.Errorf("Title should keep default, got %q", info.Title)
		}
	})
}

// versionJSONServer serves a fixed version.json payload.
func versionJSONServer(t *testing.T, status int, payload string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, payload)
	}))
}

func testUpdater(dir, remoteURL string) *Updater {
	u := NewUpdater(dir)
	u.RemoteVersionURL = remoteURL
	return u
}

func TestUpdaterCheckForUpdates(t *testing.T) {
	ctx := context.Background()

	t.Run("remote newer sets has_update", func(t *testing.T) {
		srv := versionJSONServer(t, 200, `{"version":"9.9.9","title":"T","announcement":"A","release_date":"2026-01-01","changelog":["x"],"auto_run_commands":["echo hi"],"actions_required":["z"]}`)
		defer srv.Close()
		u := testUpdater(t.TempDir(), srv.URL)
		u.Client = srv.Client()
		got := u.CheckForUpdates(ctx)
		if !got.HasUpdate {
			t.Error("HasUpdate = false, want true")
		}
		if got.RemoteVersion != "9.9.9" {
			t.Errorf("RemoteVersion = %q", got.RemoteVersion)
		}
		if got.CurrentVersion != "1.0.0" {
			t.Errorf("CurrentVersion = %q, want local default", got.CurrentVersion)
		}
		if len(got.Changelog) != 1 || got.Changelog[0] != "x" {
			t.Errorf("Changelog = %v", got.Changelog)
		}
		if got.Error != "" {
			t.Errorf("Error = %q, want empty", got.Error)
		}
	})

	t.Run("remote same version no update", func(t *testing.T) {
		srv := versionJSONServer(t, 200, `{"version":"1.0.0"}`)
		defer srv.Close()
		u := testUpdater(t.TempDir(), srv.URL)
		u.Client = srv.Client()
		got := u.CheckForUpdates(ctx)
		if got.HasUpdate {
			t.Error("HasUpdate = true, want false")
		}
	})

	t.Run("remote older no update", func(t *testing.T) {
		srv := versionJSONServer(t, 200, `{"version":"0.5.0"}`)
		defer srv.Close()
		u := testUpdater(t.TempDir(), srv.URL)
		u.Client = srv.Client()
		if got := u.CheckForUpdates(ctx); got.HasUpdate {
			t.Error("HasUpdate = true for older remote, want false")
		}
	})

	t.Run("remote error records error field", func(t *testing.T) {
		srv := versionJSONServer(t, 500, `oops`)
		defer srv.Close()
		u := testUpdater(t.TempDir(), srv.URL)
		u.Client = srv.Client()
		got := u.CheckForUpdates(ctx)
		if got.HasUpdate {
			t.Error("HasUpdate = true on 500, want false")
		}
		// 500 is not a transport error in Python either; result keeps local info.
		if got.CurrentVersion != "1.0.0" {
			t.Errorf("CurrentVersion = %q", got.CurrentVersion)
		}
	})

	t.Run("unreachable remote records error", func(t *testing.T) {
		u := testUpdater(t.TempDir(), "http://127.0.0.1:1/version.json")
		got := u.CheckForUpdates(ctx)
		if got.HasUpdate {
			t.Error("HasUpdate = true on unreachable, want false")
		}
		if got.Error == "" {
			t.Error("Error empty, want transport failure recorded")
		}
	})

	t.Run("git ahead sets has_update with log changelog", func(t *testing.T) {
		srv := versionJSONServer(t, 200, `{"version":"1.0.0"}`)
		defer srv.Close()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		stub := &stubRunner{}
		stub.runFunc = func(ctx context.Context, dir, name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.HasPrefix(joined, "fetch "):
				return "", nil
			case strings.HasPrefix(joined, "rev-list "):
				return "3\n", nil
			case strings.HasPrefix(joined, "log "):
				if !strings.Contains(joined, "-n 3") {
					t.Errorf("log called with wrong -n: %q", joined)
				}
				return "feat: x\nfix: y\n", nil
			}
			return "", fmt.Errorf("unexpected git call: %s", joined)
		}
		u := testUpdater(dir, srv.URL)
		u.Client = srv.Client()
		u.Runner = stub
		got := u.CheckForUpdates(ctx)
		if !got.HasUpdate {
			t.Error("HasUpdate = false, want true from git ahead")
		}
		if len(got.Changelog) != 2 || got.Changelog[0] != "feat: x" {
			t.Errorf("Changelog = %v, want git log lines", got.Changelog)
		}
	})

	t.Run("git fetch failure means no update", func(t *testing.T) {
		srv := versionJSONServer(t, 200, `{"version":"1.0.0"}`)
		defer srv.Close()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		stub := &stubRunner{}
		stub.runFunc = func(ctx context.Context, dir, name string, args ...string) (string, error) {
			return "", errors.New("network down")
		}
		u := testUpdater(dir, srv.URL)
		u.Client = srv.Client()
		u.Runner = stub
		if got := u.CheckForUpdates(ctx); got.HasUpdate {
			t.Error("HasUpdate = true when git fetch fails, want false")
		}
	})

	t.Run("remote newer short-circuits git", func(t *testing.T) {
		srv := versionJSONServer(t, 200, `{"version":"9.9.9"}`)
		defer srv.Close()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		stub := &stubRunner{}
		stub.runFunc = func(ctx context.Context, dir, name string, args ...string) (string, error) {
			t.Error("git should not run when remote is newer")
			return "", nil
		}
		u := testUpdater(dir, srv.URL)
		u.Client = srv.Client()
		u.Runner = stub
		if got := u.CheckForUpdates(ctx); !got.HasUpdate {
			t.Error("HasUpdate = false, want true")
		}
	})
}

func TestRenderUpdateBanner(t *testing.T) {
	info := UpdateCheckResult{
		CurrentVersion: "1.0.0",
		RemoteVersion:  "2.0.0",
		Announcement:   "Big release",
		Changelog:      []string{"a", "b", "c", "d", "e", "f"},
	}
	t.Run("ID", func(t *testing.T) {
		out := RenderUpdateBanner(info, "ID")
		for _, want := range []string{"UPDATE TERSEDIA!", "v2.0.0", "v1.0.0", "Big release", "Rincian Pembaruan"} {
			if !strings.Contains(out, want) {
				t.Errorf("banner missing %q", want)
			}
		}
		if strings.Contains(out, "NEW UPDATE AVAILABLE") {
			t.Error("ID banner leaked English headline")
		}
	})
	t.Run("EN", func(t *testing.T) {
		out := RenderUpdateBanner(info, "EN")
		if !strings.Contains(out, "NEW UPDATE AVAILABLE!") {
			t.Error("EN banner missing headline")
		}
	})
	t.Run("changelog truncated to four", func(t *testing.T) {
		out := RenderUpdateBanner(info, "ID")
		for _, item := range []string{"a", "b", "c", "d"} {
			if !strings.Contains(out, item) {
				t.Errorf("banner missing item %q", item)
			}
		}
		if strings.Contains(out, "\n   \x1b[33m•\x1b[37m e\x1b[0m") || strings.Contains(out, "•\x1b[37m e\n") {
			t.Error("banner shows more than 4 changelog items")
		}
	})
	t.Run("empty changelog shows hint", func(t *testing.T) {
		empty := info
		empty.Changelog = nil
		if out := RenderUpdateBanner(empty, "ID"); !strings.Contains(out, "Peningkatan performa") {
			t.Error("ID hint missing")
		}
		if out := RenderUpdateBanner(empty, "EN"); !strings.Contains(out, "Performance improvements") {
			t.Error("EN hint missing")
		}
	})
}

func TestFormatFullAnnouncement(t *testing.T) {
	info := VersionInfo{
		Version:         "2.0.0",
		ReleaseDate:     "2026-10-01",
		Title:           "PetaniProxy v2.0.0",
		Announcement:    "Highlights",
		Changelog:       []string{"feat a", "fix b"},
		ActionsRequired: []string{"restart once"},
	}
	out := FormatFullAnnouncement(info, "ID")
	for _, want := range []string{"v2.0.0", "2026-10-01", "PetaniProxy v2.0.0", "Highlights", "1.", "feat a", "2.", "fix b", "restart once"} {
		if !strings.Contains(out, want) {
			t.Errorf("announcement missing %q", want)
		}
	}
	// Sections omitted when empty.
	bare := FormatFullAnnouncement(VersionInfo{Version: "1.0.0"}, "ID")
	if strings.Contains(bare, "FITUR BARU") {
		t.Error("changelog section should be omitted when empty")
	}
	if strings.Contains(bare, "TINDAKAN DIPERLUKAN") {
		t.Error("actions section should be omitted when empty")
	}
}

// realExitError builds a genuine *exec.ExitError for the pull-failure path.
func realExitError(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatal("expected *exec.ExitError")
	}
	return err
}

func TestUpdaterPerformUpdate(t *testing.T) {
	ctx := context.Background()
	writeVersion := func(t *testing.T, dir, payload string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "version.json"), []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("happy path runs pull and normalized commands", func(t *testing.T) {
		dir := t.TempDir()
		writeVersion(t, dir, `{"version":"2.0.0","title":"T","announcement":"A","release_date":"2026-10-01","changelog":["c1"],"auto_run_commands":["python -m pip install -r requirements.txt","pip install foo","echo hello"]}`)
		stub := &stubRunner{}
		stub.runFunc = func(ctx context.Context, dir, name string, args ...string) (string, error) {
			return "Already up to date.\n", nil
		}
		var out strings.Builder
		u := NewUpdater(dir)
		u.Runner = stub
		u.PythonBin = "python3"
		if err := u.PerformUpdate(ctx, UpdateOptions{Stdout: &out}); err != nil {
			t.Fatalf("PerformUpdate: %v", err)
		}
		if len(stub.runCalls) != 1 || stub.runCalls[0] != "git pull origin master" {
			t.Errorf("runCalls = %v", stub.runCalls)
		}
		wantShell := []string{
			`"python3" -m pip install -r requirements.txt`,
			`"python3" -m pip install foo`,
			"echo hello",
		}
		if len(stub.shellCalls) != len(wantShell) {
			t.Fatalf("shellCalls = %v, want %v", stub.shellCalls, wantShell)
		}
		for i := range wantShell {
			if stub.shellCalls[i] != wantShell[i] {
				t.Errorf("shellCalls[%d] = %q, want %q", i, stub.shellCalls[i], wantShell[i])
			}
		}
		text := out.String()
		for _, want := range []string{"Git Pull Sukses", "SUKSES DIPERBARUI KE v2.0.0", "c1"} {
			if !strings.Contains(text, want) {
				t.Errorf("output missing %q", want)
			}
		}
	})

	t.Run("default auto command when version.json lacks it", func(t *testing.T) {
		dir := t.TempDir() // no version.json -> defaults
		stub := &stubRunner{}
		var out strings.Builder
		u := NewUpdater(dir)
		u.Runner = stub
		if err := u.PerformUpdate(ctx, UpdateOptions{Stdout: &out}); err != nil {
			t.Fatalf("PerformUpdate: %v", err)
		}
		if len(stub.shellCalls) != 1 || stub.shellCalls[0] != `"python3" -m pip install -r requirements.txt` {
			t.Errorf("shellCalls = %v", stub.shellCalls)
		}
	})

	t.Run("pull failure falls back to fetch and hard reset", func(t *testing.T) {
		dir := t.TempDir()
		stub := &stubRunner{}
		pullErr := realExitError(t)
		stub.runFunc = func(ctx context.Context, dir, name string, args ...string) (string, error) {
			if strings.Join(args, " ") == "pull origin master" {
				return "", pullErr
			}
			return "", nil
		}
		var out strings.Builder
		u := NewUpdater(dir)
		u.Runner = stub
		if err := u.PerformUpdate(ctx, UpdateOptions{Stdout: &out}); err != nil {
			t.Fatalf("PerformUpdate: %v", err)
		}
		want := []string{
			"git pull origin master",
			"git fetch origin master",
			"git reset --hard origin/master",
		}
		if len(stub.runCalls) != len(want) {
			t.Fatalf("runCalls = %v, want %v", stub.runCalls, want)
		}
		for i := range want {
			if stub.runCalls[i] != want[i] {
				t.Errorf("runCalls[%d] = %q, want %q", i, stub.runCalls[i], want[i])
			}
		}
	})

	t.Run("missing git binary returns error", func(t *testing.T) {
		dir := t.TempDir()
		stub := &stubRunner{}
		stub.runFunc = func(ctx context.Context, dir, name string, args ...string) (string, error) {
			return "", &exec.Error{Name: "git", Err: exec.ErrNotFound}
		}
		var out strings.Builder
		u := NewUpdater(dir)
		u.Runner = stub
		if err := u.PerformUpdate(ctx, UpdateOptions{Stdout: &out}); err == nil {
			t.Error("expected error when git binary missing")
		}
	})

	t.Run("restart prompt yes triggers restart func", func(t *testing.T) {
		dir := t.TempDir()
		old := restartProcessFunc
		restarted := false
		restartProcessFunc = func() error { restarted = true; return nil }
		defer func() { restartProcessFunc = old }()
		stub := &stubRunner{}
		var out strings.Builder
		u := NewUpdater(dir)
		u.Runner = stub
		opts := UpdateOptions{Stdout: &out, Stdin: strings.NewReader("y\n"), Restart: true}
		if err := u.PerformUpdate(ctx, opts); err != nil {
			t.Fatalf("PerformUpdate: %v", err)
		}
		if !restarted {
			t.Error("restart func not called on 'y'")
		}
		if !strings.Contains(out.String(), "me-restart") {
			t.Error("restart prompt not printed")
		}
	})

	t.Run("restart prompt no skips restart", func(t *testing.T) {
		dir := t.TempDir()
		old := restartProcessFunc
		restartProcessFunc = func() error { t.Error("restart must not run"); return nil }
		defer func() { restartProcessFunc = old }()
		stub := &stubRunner{}
		var out strings.Builder
		u := NewUpdater(dir)
		u.Runner = stub
		opts := UpdateOptions{Stdout: &out, Stdin: strings.NewReader("n\n"), Restart: true}
		if err := u.PerformUpdate(ctx, opts); err != nil {
			t.Fatalf("PerformUpdate: %v", err)
		}
	})

	t.Run("restart disabled never prompts", func(t *testing.T) {
		dir := t.TempDir()
		old := restartProcessFunc
		restartProcessFunc = func() error { t.Error("restart must not run"); return nil }
		defer func() { restartProcessFunc = old }()
		stub := &stubRunner{}
		var out strings.Builder
		u := NewUpdater(dir)
		u.Runner = stub
		opts := UpdateOptions{Stdout: &out, Stdin: strings.NewReader("y\n")}
		if err := u.PerformUpdate(ctx, opts); err != nil {
			t.Fatalf("PerformUpdate: %v", err)
		}
		if strings.Contains(out.String(), "[Y/n]") {
			t.Error("prompt printed although Restart=false")
		}
	})
}
