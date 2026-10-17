package petaniproxy

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

func testCLI(in string) (*CLI, *strings.Builder) {
	var out strings.Builder
	return NewCLI(strings.NewReader(in), &out), &out
}

func plainCLI(in string) (*CLI, *strings.Builder) {
	c, out := testCLI(in)
	c.NoColor = true
	c.Width = 78
	return c, out
}

func TestTUIVisibleLen(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"plain ascii", "hello", 5},
		{"empty", "", 0},
		{"strips color codes", "\x1b[32m● OK\x1b[0m", 4},
		{"unicode counts as runes", "╭─╮", 3},
		{"only escapes", "\x1b[1m\x1b[0m", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VisibleLen(tt.input); got != tt.want {
				t.Errorf("VisibleLen(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestTUIPadTo(t *testing.T) {
	tests := []struct {
		name  string
		input string
		width int
		want  string
		wantW int
	}{
		{"pads short text", "ab", 5, "ab   ", 5},
		{"exact width untouched", "abc", 3, "abc", 3},
		{"long text untouched", "abcdef", 3, "abcdef", 6},
		{"ansi-aware pad", "\x1b[32mab\x1b[0m", 5, "\x1b[32mab\x1b[0m   ", 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PadTo(tt.input, tt.width)
			if got != tt.want {
				t.Errorf("PadTo(%q, %d) = %q, want %q", tt.input, tt.width, got, tt.want)
			}
			if w := VisibleLen(got); w != tt.wantW {
				t.Errorf("visible width = %d, want %d", w, tt.wantW)
			}
		})
	}
}

func TestTUITerminalWidth(t *testing.T) {
	tests := []struct {
		name string
		def  int
		want int
	}{
		{"default 78 kept", 78, 78},
		{"clamped to minimum 72", 40, 72},
		{"clamped to maximum 96", 200, 96},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TerminalWidth(tt.def); got != tt.want {
				t.Errorf("TerminalWidth(%d) = %d, want %d", tt.def, got, tt.want)
			}
		})
	}
}

func TestTUIReadKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"keyword up", "up\n", KeyUp},
		{"keyword down", "DOWN\n", KeyDown},
		{"keyword enter", "enter\n", KeyEnter},
		{"keyword esc", "esc\n", KeyEsc},
		{"keyword space", "space\n", KeySpace},
		{"keyword backspace", "backspace\n", KeyBackspace},
		{"bare enter", "\n", KeyEnter},
		{"single char lowercased", "E\n", "e"},
		{"digit hotkey", "1\n", "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadKey(bufio.NewReader(strings.NewReader(tt.input)))
			if err != nil {
				t.Fatalf("ReadKey() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ReadKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}

	t.Run("eof returns error", func(t *testing.T) {
		if _, err := ReadKey(bufio.NewReader(strings.NewReader(""))); err == nil {
			t.Error("ReadKey() on empty input should return an error")
		}
	})
}

func TestTUIRenderBadge(t *testing.T) {
	tests := []struct {
		name   string
		status string
		glyph  string
	}{
		{"ok green", "OK", "●"},
		{"active green", "ACTIVE", "●"},
		{"siap green", "SIAP", "●"},
		{"gacor green", "GACOR", "●"},
		{"ultra green", "ULTRA", "●"},
		{"warn yellow", "WARN", "▲"},
		{"pending yellow", "PENDING", "▲"},
		{"exp yellow", "EXP", "▲"},
		{"err red", "ERR", "○"},
		{"offline red", "OFFLINE", "○"},
		{"fail red", "FAIL", "○"},
		{"belum red", "BELUM", "○"},
		{"unknown dim", "???", "○"},
		{"case insensitive", "ok", "●"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := plainCLI("")
			got := c.RenderBadge(tt.status, "teks")
			if !strings.Contains(got, tt.glyph+" teks") {
				t.Errorf("RenderBadge(%q) = %q, want glyph %q", tt.status, got, tt.glyph)
			}
		})
	}

	t.Run("colored output wraps ansi", func(t *testing.T) {
		c, _ := testCLI("")
		got := c.RenderBadge("OK", "x")
		if !strings.Contains(got, "\x1b[32m") || !strings.Contains(got, "\x1b[0m") {
			t.Errorf("expected green ANSI wrap, got %q", got)
		}
	})
}

func TestTUIBuildHeader(t *testing.T) {
	c, _ := plainCLI("")
	got := c.BuildHeader("PETANI PROXY", "subtitle here", "1.2.0")
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("header should have 3 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "PETANI PROXY v1.2.0") {
		t.Errorf("top line missing label: %q", lines[0])
	}
	if !strings.Contains(lines[1], "subtitle here") {
		t.Errorf("subtitle line missing subtitle: %q", lines[1])
	}
	for i, ln := range lines {
		if w := VisibleLen(ln); w != 78 {
			t.Errorf("line %d visible width = %d, want 78: %q", i, w, ln)
		}
	}

	t.Run("no version omits tag", func(t *testing.T) {
		got := c.BuildHeader("PETANI PROXY", "sub", "")
		if strings.Contains(got, "v1") || strings.Contains(got, "PETANI PROXY v") {
			t.Errorf("unexpected version tag in %q", got)
		}
		if !strings.Contains(got, "PETANI PROXY") {
			t.Errorf("label missing in %q", got)
		}
	})
}

func TestTUIBuildMetricsCard(t *testing.T) {
	c, _ := plainCLI("")
	metrics := []MetricPair{
		{Col1Label: "Total", Col1Val: "120", Col2Label: "Aktif", Col2Val: "98"},
		{Col1Label: "Gagal", Col1Val: "3", Col2Label: "", Col2Val: ""},
	}
	got := c.BuildMetricsCard("METRIK", metrics)
	if !strings.Contains(got, "METRIK") {
		t.Errorf("title missing: %q", got)
	}
	for _, want := range []string{"Total", "120", "Aktif", "98", "Gagal", "3"} {
		if !strings.Contains(got, want) {
			t.Errorf("metric %q missing in output:\n%s", want, got)
		}
	}
	for i, ln := range strings.Split(got, "\n") {
		if w := VisibleLen(ln); w != 78 {
			t.Errorf("line %d visible width = %d, want 78: %q", i, w, ln)
		}
	}
}

func TestTUIBuildInfoCard(t *testing.T) {
	c, _ := plainCLI("")
	rows := []InfoRow{{Key: "Port", Val: "8888"}, {Key: "Protokol", Val: "http"}}
	got := c.BuildInfoCard("INFO", rows, ColorCyan)
	if !strings.Contains(got, "INFO") || !strings.Contains(got, "Port") || !strings.Contains(got, "8888") {
		t.Errorf("info card missing content:\n%s", got)
	}
	for i, ln := range strings.Split(got, "\n") {
		if w := VisibleLen(ln); w != 78 {
			t.Errorf("line %d visible width = %d, want 78: %q", i, w, ln)
		}
	}
}

func TestTUIBuildTable(t *testing.T) {
	c, _ := plainCLI("")

	t.Run("basic table with title", func(t *testing.T) {
		got := c.BuildTable(
			[]string{"Proxy", "Negara"},
			[][]string{{"1.2.3.4:8080", "ID"}, {"5.6.7.8:3128", "SG"}},
			[]string{"left", "center"},
			"DAFTAR PROXY",
		)
		for _, want := range []string{"DAFTAR PROXY", "Proxy", "Negara", "1.2.3.4:8080", "ID", "SG"} {
			if !strings.Contains(got, want) {
				t.Errorf("table missing %q:\n%s", want, got)
			}
		}
		lines := strings.Split(got, "\n")
		if !strings.HasPrefix(lines[0], "╭─") {
			t.Errorf("title box should start the table, got %q", lines[0])
		}
		for i, ln := range lines {
			if w := VisibleLen(ln); w > 78 {
				t.Errorf("line %d width %d exceeds 78: %q", i, w, ln)
			}
		}
	})

	t.Run("empty headers returns empty", func(t *testing.T) {
		if got := c.BuildTable(nil, nil, nil, ""); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("long cell truncated with ellipsis", func(t *testing.T) {
		c2, _ := plainCLI("")
		c2.Width = 40
		long := strings.Repeat("x", 100)
		got := c2.BuildTable([]string{"H"}, [][]string{{long}}, nil, "")
		if !strings.Contains(got, "…") {
			t.Errorf("expected ellipsis truncation:\n%s", got)
		}
		for i, ln := range strings.Split(got, "\n") {
			if w := VisibleLen(ln); w > 40 {
				t.Errorf("line %d width %d exceeds 40: %q", i, w, ln)
			}
		}
	})

	t.Run("right alignment", func(t *testing.T) {
		got := c.BuildTable([]string{"Num"}, [][]string{{"7"}}, []string{"right"}, "")
		found := false
		for _, ln := range strings.Split(got, "\n") {
			// column width 3 ("Num"), right-aligned "7" -> cell "   7 "
			if strings.Contains(ln, "│   7 │") {
				found = true
			}
		}
		if !found {
			t.Errorf("right-aligned cell not found:\n%s", got)
		}
	})
}

func TestTUIMenuAddItem(t *testing.T) {
	m := &Menu{}
	m.AddSection("SEKSI")
	m.AddItem("E", "Ekspor", "desc", "badge")
	if len(m.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(m.Items))
	}
	if !m.Items[0].IsHeader {
		t.Error("first item should be a header")
	}
	it := m.Items[1]
	if it.Key != "e" {
		t.Errorf("key should be lowercased, got %q", it.Key)
	}
	if it.Label != "Ekspor" || it.Desc != "desc" || it.Badge != "badge" {
		t.Errorf("unexpected item fields: %+v", it)
	}
	if got := len(m.selectableIndices()); got != 1 {
		t.Errorf("selectableIndices = %d, want 1", got)
	}
}

func buildTestMenu() *Menu {
	m := &Menu{Title: "PETANI PROXY", Subtitle: "sub", Version: "1.2.0"}
	m.AddSection("S1")
	m.AddItem("w", "Webshare", "desc w", "")
	m.AddItem("e", "Ekspor", "desc e", "")
	m.AddItem("0", "Keluar", "desc 0", "")
	return m
}

func TestTUIRenderMenu(t *testing.T) {
	c, _ := plainCLI("")
	got := c.RenderMenu(buildTestMenu())
	for _, want := range []string{
		"PETANI PROXY v1.2.0", "S1", "[W]", "[E]", "[0]",
		"Webshare", "Ekspor", "Keluar", "↑/↓: Navigate",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("menu missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "❯") {
		t.Error("selected item cursor ❯ missing")
	}
}

func TestCLIShowMenu(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"enter selects first", "\n", "w"},
		{"hotkey instant action", "e\n", "e"},
		{"hotkey uppercase", "E\n", "e"},
		{"q exits", "q\n", "0"},
		{"esc exits", "esc\n", "0"},
		{"zero exits", "0\n", "0"},
		{"j moves down then enter", "j\n\n", "e"},
		{"k wraps to last then enter", "k\n\n", "0"},
		{"down arrow then enter", "down\n\n", "e"},
		{"unknown key ignored then hotkey", "zzz\n" + "e\n", "e"},
		{"eof exits", "", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := plainCLI(tt.input)
			got, err := c.ShowMenu(buildTestMenu())
			if err != nil {
				t.Fatalf("ShowMenu() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ShowMenu() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("empty menu returns empty", func(t *testing.T) {
		c, _ := plainCLI("\n")
		got, err := c.ShowMenu(&Menu{})
		if err != nil {
			t.Fatalf("ShowMenu() error = %v", err)
		}
		if got != "" {
			t.Errorf("ShowMenu() = %q, want empty", got)
		}
	})
}

func TestCLIRunCommand(t *testing.T) {
	t.Run("registered handler runs", func(t *testing.T) {
		c, _ := plainCLI("")
		called := false
		c.RegisterCommand("e", func() error { called = true; return nil })
		if err := c.RunCommand("E"); err != nil {
			t.Fatalf("RunCommand() error = %v", err)
		}
		if !called {
			t.Error("handler was not called")
		}
	})

	t.Run("handler error propagates", func(t *testing.T) {
		c, _ := plainCLI("")
		boom := errors.New("boom")
		c.RegisterCommand("t", func() error { return boom })
		if err := c.RunCommand("t"); !errors.Is(err, boom) {
			t.Errorf("RunCommand() = %v, want %v", err, boom)
		}
	})

	t.Run("exit keys return ErrExit", func(t *testing.T) {
		c, _ := plainCLI("")
		for _, key := range []string{"0", "q", "Q"} {
			if err := c.RunCommand(key); !errors.Is(err, ErrExit) {
				t.Errorf("RunCommand(%q) = %v, want ErrExit", key, err)
			}
		}
	})

	t.Run("unknown command returns UnknownCommandError", func(t *testing.T) {
		c, _ := plainCLI("")
		err := c.RunCommand("zzz")
		var uce *UnknownCommandError
		if !errors.As(err, &uce) {
			t.Fatalf("RunCommand() = %T %v, want *UnknownCommandError", err, err)
		}
		if uce.Command != "zzz" {
			t.Errorf("Command = %q, want %q", uce.Command, "zzz")
		}
	})
}

func TestCLIQuickConfirm(t *testing.T) {
	tests := []struct {
		name string
		in   string
		def  bool
		want bool
	}{
		{"enter takes default true", "\n", true, true},
		{"enter takes default false", "\n", false, false},
		{"y answers yes", "y\n", false, true},
		{"n answers no", "n\n", true, false},
		{"esc answers no", "esc\n", true, false},
		{"other key takes default", "x\n", true, true},
		{"eof takes default", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, out := plainCLI(tt.in)
			if got := c.QuickConfirm("Lanjut?", tt.def); got != tt.want {
				t.Errorf("QuickConfirm() = %v, want %v", got, tt.want)
			}
			if !strings.Contains(out.String(), "Lanjut?") {
				t.Errorf("prompt missing in output: %q", out.String())
			}
		})
	}

	t.Run("hint reflects default", func(t *testing.T) {
		c, out := plainCLI("\n")
		c.QuickConfirm("P?", true)
		if !strings.Contains(out.String(), "[Y/n]") {
			t.Errorf("expected [Y/n] hint: %q", out.String())
		}
		c2, out2 := plainCLI("\n")
		c2.QuickConfirm("P?", false)
		if !strings.Contains(out2.String(), "[y/N]") {
			t.Errorf("expected [y/N] hint: %q", out2.String())
		}
	})
}

func TestCLIQuickPause(t *testing.T) {
	t.Run("default message", func(t *testing.T) {
		c, out := plainCLI("\n")
		c.QuickPause("")
		if !strings.Contains(out.String(), "Tekan tombol apa saja untuk melanjutkan...") {
			t.Errorf("default pause message missing: %q", out.String())
		}
	})

	t.Run("custom message", func(t *testing.T) {
		c, out := plainCLI("x\n")
		c.QuickPause("lanjut bro")
		if !strings.Contains(out.String(), "lanjut bro") {
			t.Errorf("custom message missing: %q", out.String())
		}
	})
}

func TestCLIShowStatus(t *testing.T) {
	c, out := plainCLI("")
	c.ShowStatus(&StatusView{
		Title:        "PETANI PROXY",
		Subtitle:     "status sub",
		Version:      "1.2.0",
		UpdateNotice: "update tersedia",
		MetricsTitle: "METRIK",
		Metrics:      []MetricPair{{Col1Label: "Total", Col1Val: "5", Col2Label: "OK", Col2Val: "4"}},
		InfoTitle:    "INFO",
		Info:         []InfoRow{{Key: "Port", Val: "8888"}},
	})
	got := out.String()
	for _, want := range []string{
		"PETANI PROXY v1.2.0", "status sub", "update tersedia",
		"METRIK", "Total", "INFO", "Port", "8888",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
}

func TestTUIClearScreen(t *testing.T) {
	var b strings.Builder
	ClearScreen(&b)
	if b.String() != "\x1b[2J\x1b[H" {
		t.Errorf("ClearScreen wrote %q", b.String())
	}
}
