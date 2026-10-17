// Command-line interface for petani-proxy, ported from core/tui.py.
//
// The Python original is a full keyboard-driven terminal UI (raw keypress
// reading, arrow-key menu navigation, colorama styling). This Go port keeps
// every screen/command and its semantics but as a simple, testable CLI:
// input comes from an io.Reader (line-based key names such as "up", "enter"
// or single-character hotkeys) and all output goes to an io.Writer, so no
// code here touches os.Stdin/os.Stdout directly.
//
// Petani-proxy command vocabulary (kept from main.py):
//
//	w  Webshare Residential harvester        r  Grok xAI account farm
//	c  Cloudflare WARP local tunnel          f  Fast async harvester
//	d  Web dashboard                         g  24/7 farmer daemon
//	1  Bot breeder profile                   2  High-concurrency scraper profile
//	3  Ultra-low latency profile             e  Export clean pool
//	t  Identity leak check                   s  Local ammo storage
//	k  Quick settings                        u  Check for updates
//	m  Manual filter lab                     l  Switch language
//	0  Exit
package petaniproxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Key code constants, mirroring tui.py's KEY_* constants.
const (
	KeyUp        = "up"
	KeyDown      = "down"
	KeyLeft      = "left"
	KeyRight     = "right"
	KeyEnter     = "enter"
	KeyEsc       = "esc"
	KeySpace     = "space"
	KeyBackspace = "backspace"
)

// ANSI color palette used by the original colorama-based UI.
const (
	ColorReset  = "\x1b[0m"
	ColorBold   = "\x1b[1m"
	ColorRed    = "\x1b[31m"
	ColorGreen  = "\x1b[32m"
	ColorYellow = "\x1b[33m"
	ColorCyan   = "\x1b[36m"
	ColorWhite  = "\x1b[37m"
	// ColorDim maps colorama's Fore.LIGHTBLACK_EX (dim gray).
	ColorDim = "\x1b[90m"
)

// ErrExit is returned by RunCommand when the user picks the exit command
// ("0" or "q"), mirroring InteractiveMenu.run returning "0".
var ErrExit = errors.New("petaniproxy: exit requested")

// UnknownCommandError reports a command key with no registered handler.
type UnknownCommandError struct {
	// Command is the raw key the user entered.
	Command string
}

// Error implements the error interface.
func (e *UnknownCommandError) Error() string {
	return fmt.Sprintf("petaniproxy: unknown command %q", e.Command)
}

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// VisibleLen returns the visible width of text, ignoring ANSI color escapes.
// Mirrors tui.py's visible_len; widths are counted in runes like Python's len().
func VisibleLen(text string) int {
	return len([]rune(ansiRe.ReplaceAllString(text, "")))
}

// PadTo pads text with trailing spaces until its visible width reaches
// targetWidth. Mirrors tui.py's pad_to.
func PadTo(text string, targetWidth int) string {
	if vlen := VisibleLen(text); vlen < targetWidth {
		return text + strings.Repeat(" ", targetWidth-vlen)
	}
	return text
}

// TerminalWidth returns the render width for CLI output. The original queried
// the real terminal; stdlib-only Go cannot do that portably, so this returns
// defaultWidth clamped to [72, 96], exactly like tui.py's get_terminal_width.
func TerminalWidth(defaultWidth int) int {
	return max(72, min(defaultWidth, 96))
}

// ClearScreen writes the ANSI clear-screen + cursor-home sequence to w.
// Mirrors tui.py's clear_screen (non-Windows branch).
func ClearScreen(w io.Writer) {
	fmt.Fprint(w, "\x1b[2J\x1b[H")
}

// ReadKey reads one line from r and translates it into a key name:
// "up", "down", "left", "right", "enter", "esc", "space", "backspace",
// or a single lowercased character (hotkey). An empty line means "enter".
// This is the line-based equivalent of tui.py's raw read_key, which keeps
// the CLI scriptable and unit-testable.
func ReadKey(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return KeyEnter, nil
	}
	switch lower := strings.ToLower(line); lower {
	case KeyUp, KeyDown, KeyLeft, KeyRight, KeyEnter, KeyEsc, KeySpace, KeyBackspace:
		return lower, nil
	default:
		return lower, nil
	}
}

// MetricPair is one row of a two-column metrics card:
// (col1 label, col1 value, col2 label, col2 value). Mirrors the tuple rows
// passed to tui.py's build_metrics_card.
type MetricPair struct {
	Col1Label string
	Col1Val   string
	Col2Label string
	Col2Val   string
}

// InfoRow is one key/value row of an info card. Mirrors the tuple rows
// passed to tui.py's build_info_card.
type InfoRow struct {
	Key string
	Val string
}

// MenuItem is a single selectable entry (or section header) of a Menu.
// Mirrors tui.py's MenuItem.
type MenuItem struct {
	// Key is the hotkey, stored lowercase ("e", "1", ...).
	Key string
	// Label is the display label.
	Label string
	// Desc is the dimmed description shown beside the label.
	Desc string
	// Badge is an optional pre-rendered status badge.
	Badge string
	// IsHeader marks a non-selectable section header.
	IsHeader bool
}

// Menu is an interactive keyboard-driven menu. Mirrors tui.py's
// InteractiveMenu.
type Menu struct {
	Title        string
	Subtitle     string
	Version      string
	MetricsTitle string
	Metrics      []MetricPair
	UpdateNotice string
	Items        []MenuItem
	selected     int
}

// AddSection appends a non-selectable section header to the menu.
func (m *Menu) AddSection(name string) {
	m.Items = append(m.Items, MenuItem{Label: name, IsHeader: true})
}

// AddItem appends a selectable item to the menu. The key is lowercased,
// mirroring MenuItem.__init__.
func (m *Menu) AddItem(key, label, desc, badge string) {
	m.Items = append(m.Items, MenuItem{
		Key:   strings.ToLower(key),
		Label: label,
		Desc:  desc,
		Badge: badge,
	})
}

// selectableIndices returns the indices of non-header items.
func (m *Menu) selectableIndices() []int {
	var idx []int
	for i, it := range m.Items {
		if !it.IsHeader {
			idx = append(idx, i)
		}
	}
	return idx
}

// StatusView bundles everything ShowStatus renders: header, metrics card
// and info card.
type StatusView struct {
	Title        string
	Subtitle     string
	Version      string
	UpdateNotice string
	MetricsTitle string
	Metrics      []MetricPair
	InfoTitle    string
	Info         []InfoRow
}

// CLI is the simple command-line interface for petani-proxy. It mirrors the
// TUI screens as methods (ShowStatus, ShowMenu, RunCommand) while reading
// from In and writing to Out so every interaction is unit-testable.
type CLI struct {
	// In is the keystroke/line input source.
	In io.Reader
	// Out is the render output destination.
	Out io.Writer
	// Width overrides the render width; 0 means TerminalWidth(78).
	Width int
	// NoColor disables ANSI color output (useful for tests and pipes).
	NoColor bool

	handlers map[string]func() error
}

// NewCLI builds a CLI reading from in and writing to out.
func NewCLI(in io.Reader, out io.Writer) *CLI {
	return &CLI{In: in, Out: out, handlers: map[string]func() error{}}
}

// RegisterCommand binds a command key (e.g. "e", "t", "1") to its handler
// so RunCommand can dispatch it. The key is matched case-insensitively.
func (c *CLI) RegisterCommand(key string, handler func() error) {
	if c.handlers == nil {
		c.handlers = map[string]func() error{}
	}
	c.handlers[strings.ToLower(key)] = handler
}

// RunCommand dispatches a menu command key. "0" and "q" return ErrExit
// (mirroring InteractiveMenu.run returning "0"); an unregistered key
// returns *UnknownCommandError.
func (c *CLI) RunCommand(cmd string) error {
	switch key := strings.ToLower(cmd); key {
	case "0", "q":
		return ErrExit
	default:
		if h, ok := c.handlers[key]; ok {
			return h()
		}
		return &UnknownCommandError{Command: cmd}
	}
}

// width resolves the effective render width.
func (c *CLI) width() int {
	if c.Width > 0 {
		return c.Width
	}
	return TerminalWidth(78)
}

// paint wraps text in an ANSI color sequence unless NoColor is set.
func (c *CLI) paint(color, text string) string {
	if c.NoColor {
		return text
	}
	return color + text + ColorReset
}

// write renders s to the CLI output.
func (c *CLI) write(s string) {
	fmt.Fprint(c.Out, s)
}

// RenderBadge formats a clean status badge with a minimal glyph, mirroring
// tui.py's render_badge keyword groups: green for OK/ACTIVE/READY/ONLINE/
// SIAP/GACOR/ULTRA, yellow for WARN/PENDING/UPDATE/EXP, red for
// ERR/OFF/OFFLINE/NONE/FAIL/BELUM, dim gray otherwise.
func (c *CLI) RenderBadge(status, text string) string {
	s := strings.ToUpper(status)
	containsAny := func(keys ...string) bool {
		for _, k := range keys {
			if strings.Contains(s, k) {
				return true
			}
		}
		return false
	}
	switch {
	case containsAny("OK", "ACTIVE", "READY", "ONLINE", "SIAP", "GACOR", "ULTRA"):
		return c.paint(ColorGreen, "● "+text)
	case containsAny("WARN", "PENDING", "UPDATE", "EXP"):
		return c.paint(ColorYellow, "▲ "+text)
	case containsAny("ERR", "OFF", "OFFLINE", "NONE", "FAIL", "BELUM"):
		return c.paint(ColorRed, "○ "+text)
	default:
		return c.paint(ColorDim, "○ "+text)
	}
}

// BuildHeader builds the rounded "PETANI PROXY v<version>" header box,
// mirroring tui.py's build_header.
func (c *CLI) BuildHeader(title, subtitle, version string) string {
	width := c.width()
	innerW := width - 2
	verTag := ""
	if version != "" {
		verTag = "v" + version
	}
	headerLabel := strings.TrimSpace("PETANI PROXY " + verTag)

	topLine := "╭─ " + c.paint(ColorWhite+ColorBold, headerLabel) + c.paint(ColorCyan, " ")
	topRem := innerW - VisibleLen(topLine) + 1
	borderTop := c.paint(ColorCyan, topLine+strings.Repeat("─", max(2, topRem))+"╮")

	subLine := c.paint(ColorCyan, "│") + c.paint(ColorDim, PadTo("  "+subtitle, innerW)) + c.paint(ColorCyan, "│")
	borderBot := c.paint(ColorCyan, "╰"+strings.Repeat("─", innerW)+"╯")
	_ = title // title kept for API parity with build_header's title arg
	return borderTop + "\n" + subLine + "\n" + borderBot
}

// BuildMetricsCard builds a 2-column metrics card with aligned borders,
// mirroring tui.py's build_metrics_card.
func (c *CLI) BuildMetricsCard(title string, metrics []MetricPair) string {
	width := c.width()
	innerW := width - 2
	colW := innerW / 2

	topLine := "╭─ " + c.paint(ColorWhite+ColorBold, title) + c.paint(ColorDim, " ")
	topRem := innerW - VisibleLen(topLine) + 1
	lines := []string{c.paint(ColorDim, topLine+strings.Repeat("─", max(2, topRem))+"╮")}

	for _, mp := range metrics {
		col1 := "  " + c.paint(ColorDim, fmt.Sprintf("%-15s", mp.Col1Label)) + mp.Col1Val
		col2 := ""
		if mp.Col2Label != "" {
			col2 = " " + c.paint(ColorDim, fmt.Sprintf("%-14s", mp.Col2Label)) + mp.Col2Val
		}
		row := c.paint(ColorDim, "│") + PadTo(col1, colW) + PadTo(col2, innerW-colW) + c.paint(ColorDim, "│")
		lines = append(lines, row)
	}
	lines = append(lines, c.paint(ColorDim, "╰"+strings.Repeat("─", innerW)+"╯"))
	return strings.Join(lines, "\n")
}

// BuildInfoCard builds a clean key-value summary card with rounded borders,
// mirroring tui.py's build_info_card. borderColor takes one of the Color*
// constants (ColorCyan by default in the original).
func (c *CLI) BuildInfoCard(title string, rows []InfoRow, borderColor string) string {
	width := c.width()
	innerW := width - 2

	topLine := "╭─ " + c.paint(ColorWhite+ColorBold, title) + c.paint(borderColor, " ")
	topRem := innerW - VisibleLen(topLine) + 1
	lines := []string{c.paint(borderColor, topLine+strings.Repeat("─", max(2, topRem))+"╮")}

	for _, r := range rows {
		content := "  " + c.paint(ColorDim, fmt.Sprintf("%-22s", r.Key)) + r.Val
		lines = append(lines, c.paint(borderColor, "│")+PadTo(content, innerW)+c.paint(borderColor, "│"))
	}
	lines = append(lines, c.paint(borderColor, "╰"+strings.Repeat("─", innerW)+"╯"))
	return strings.Join(lines, "\n")
}

// tableWidths computes per-column content widths, shrinking the largest
// columns proportionally when the table would exceed width.
func tableWidths(headers []string, rows [][]string, width int) []int {
	numCols := len(headers)
	colWidths := make([]int, numCols)
	for i, h := range headers {
		colWidths[i] = VisibleLen(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < numCols && VisibleLen(cell) > colWidths[i] {
				colWidths[i] = VisibleLen(cell)
			}
		}
	}
	innerWidths := make([]int, numCols)
	sum := 0
	for i, cw := range colWidths {
		innerWidths[i] = cw + 2
		sum += innerWidths[i]
	}
	if total := sum + numCols + 1; total > width {
		scale := float64(width-(numCols+1)) / float64(max(1, sum))
		for i, iw := range innerWidths {
			innerWidths[i] = max(4, int(float64(iw)*scale))
		}
	}
	for i, iw := range innerWidths {
		colWidths[i] = iw - 2
	}
	return colWidths
}

// tableBorder renders a horizontal table border line.
func tableBorder(c *CLI, left, mid, right string, colWidths []int) string {
	parts := make([]string, len(colWidths))
	for i, cw := range colWidths {
		parts[i] = strings.Repeat("─", cw+2)
	}
	return c.paint(ColorDim, left+strings.Join(parts, mid)+right)
}

// truncateRunes cuts s to at most n runes, like Python's s[:n].
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// tableCell pads or truncates a cell to the column width honoring alignment.
func tableCell(val string, cw, alignIdx int, aligns []string) string {
	vlen := VisibleLen(val)
	if vlen > cw {
		val = truncateRunes(val, max(1, cw-1)) + "…"
		vlen = VisibleLen(val)
	}
	align := "left"
	if alignIdx < len(aligns) {
		align = aligns[alignIdx]
	}
	var padded string
	switch align {
	case "right":
		padded = strings.Repeat(" ", cw-vlen) + val
	case "center":
		left := (cw - vlen) / 2
		padded = strings.Repeat(" ", left) + val + strings.Repeat(" ", cw-vlen-left)
	default:
		padded = val + strings.Repeat(" ", cw-vlen)
	}
	return " " + padded + " "
}

// BuildTable builds a Unicode table with rounded corners, mirroring
// tui.py's build_table (ANSI-aware widths, left/right/center alignment,
// proportional column shrinking, optional title box).
func (c *CLI) BuildTable(headers []string, rows [][]string, aligns []string, title string) string {
	width := c.width()
	numCols := len(headers)
	if numCols == 0 {
		return ""
	}
	if aligns == nil {
		aligns = make([]string, numCols)
		for i := range aligns {
			aligns[i] = "left"
		}
	}
	colWidths := tableWidths(headers, rows, width)
	pipe := c.paint(ColorDim, "│")

	var lines []string
	if title != "" {
		titleTop := "╭─ " + c.paint(ColorWhite+ColorBold, title) + c.paint(ColorDim, " ")
		totalInner := 0
		for _, cw := range colWidths {
			totalInner += cw + 2
		}
		rem := totalInner + numCols - 1 - VisibleLen(titleTop)
		lines = append(lines, c.paint(ColorDim, titleTop+strings.Repeat("─", max(2, rem))+"╮"))
		lines = append(lines, tableBorder(c, "├", "┬", "┤", colWidths))
	} else {
		lines = append(lines, tableBorder(c, "╭", "┬", "╮", colWidths))
	}

	headerCells := make([]string, numCols)
	for i, h := range headers {
		headerCells[i] = tableCell(c.paint(ColorCyan+ColorBold, PadTo(h, colWidths[i])), colWidths[i], i, aligns)
	}
	lines = append(lines, pipe+strings.Join(headerCells, pipe)+pipe)
	lines = append(lines, tableBorder(c, "├", "┼", "┤", colWidths))

	for _, row := range rows {
		cells := make([]string, numCols)
		for i := 0; i < numCols; i++ {
			val := ""
			if i < len(row) {
				val = row[i]
			}
			cells[i] = tableCell(val, colWidths[i], i, aligns)
		}
		lines = append(lines, pipe+strings.Join(cells, pipe)+pipe)
	}
	lines = append(lines, tableBorder(c, "╰", "┴", "╯", colWidths))
	return strings.Join(lines, "\n")
}

// normalizeSelection moves m.selected onto a selectable item (first one if
// the current selection is a header or out of range), mirroring the guard at
// the top of InteractiveMenu.run.
func (m *Menu) normalizeSelection() {
	selectable := m.selectableIndices()
	if len(selectable) == 0 {
		return
	}
	for _, idx := range selectable {
		if idx == m.selected {
			return
		}
	}
	m.selected = selectable[0]
}

// RenderMenu renders the full menu frame (header, notice, metrics, items,
// footer) as a string without reading input, mirroring
// InteractiveMenu.render.
func (c *CLI) RenderMenu(m *Menu) string {
	m.normalizeSelection()
	var b strings.Builder
	b.WriteString(c.BuildHeader(m.Title, m.Subtitle, m.Version))
	b.WriteString("\n\n")
	if m.UpdateNotice != "" {
		b.WriteString("  " + c.paint(ColorYellow, "▲ "+m.UpdateNotice) + "\n\n")
	}
	if len(m.Metrics) > 0 {
		b.WriteString(c.BuildMetricsCard(m.MetricsTitle, m.Metrics))
		b.WriteString("\n\n")
	}
	for i, item := range m.Items {
		if item.IsHeader {
			fmt.Fprintf(&b, "\n  %s\n", c.paint(ColorDim+ColorBold, strings.ToUpper(item.Label)))
			continue
		}
		isSelected := i == m.selected
		cursor := "  "
		if isSelected {
			cursor = c.paint(ColorCyan+ColorBold, "❯") + " "
		}
		keyColor := ColorDim
		labelColor := ColorWhite
		if isSelected {
			keyColor = ColorCyan
			labelColor = ColorWhite + ColorBold
		}
		keyTag := c.paint(keyColor, "["+strings.ToUpper(item.Key)+"]")
		label := c.paint(labelColor, fmt.Sprintf("%-23s", item.Label))
		badge := ""
		if item.Badge != "" {
			badge = " " + item.Badge
		}
		desc := ""
		if item.Desc != "" {
			desc = c.paint(ColorDim, item.Desc)
		}
		fmt.Fprintf(&b, "%s%s %s%s  %s\n", cursor, keyTag, label, badge, desc)
	}
	b.WriteString("\n")
	b.WriteString(c.paint(ColorDim, "  ↑/↓: Navigate  •  Enter: Select  •  [Key]: Instant Action  •  Esc/q: Exit"))
	return b.String()
}

// ShowMenu renders the menu and reads keys until the user picks a command.
// Arrow keys (or k/j) move the highlight, Enter/Space selects, Esc/q exits.
// Returns the selected item's key, or "0" on exit (mirroring
// InteractiveMenu.run returning "0").
func (c *CLI) ShowMenu(m *Menu) (string, error) {
	selectable := m.selectableIndices()
	if len(selectable) == 0 {
		return "", nil
	}
	m.normalizeSelection()

	r := bufio.NewReader(c.In)
	for {
		c.write(c.RenderMenu(m) + "\n")
		key, err := ReadKey(r)
		if err != nil {
			return "0", nil // EOF / closed input behaves like Esc
		}
		switch key {
		case KeyUp, "k":
			pos := indexOf(selectable, m.selected)
			m.selected = selectable[(pos-1+len(selectable))%len(selectable)]
		case KeyDown, "j":
			pos := indexOf(selectable, m.selected)
			m.selected = selectable[(pos+1)%len(selectable)]
		case KeyEnter, KeySpace:
			return m.Items[m.selected].Key, nil
		case KeyEsc, "q", "0":
			return "0", nil
		default:
			if key == "" {
				continue
			}
			for _, idx := range selectable {
				if m.Items[idx].Key == key {
					return key, nil
				}
			}
		}
	}
}

// indexOf returns the position of v in s, or -1.
func indexOf(s []int, v int) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// ShowStatus renders a status screen: header, optional update notice,
// metrics card and info card. Mirrors the status-display usage of the
// build_* helpers in main.py.
func (c *CLI) ShowStatus(v *StatusView) {
	c.write(c.BuildHeader(v.Title, v.Subtitle, v.Version) + "\n\n")
	if v.UpdateNotice != "" {
		c.write("  " + c.paint(ColorYellow, "▲ "+v.UpdateNotice) + "\n\n")
	}
	if len(v.Metrics) > 0 {
		c.write(c.BuildMetricsCard(v.MetricsTitle, v.Metrics) + "\n\n")
	}
	if len(v.Info) > 0 {
		c.write(c.BuildInfoCard(v.InfoTitle, v.Info, ColorCyan) + "\n")
	}
}

// QuickConfirm asks a yes/no question; the user taps y/n or Enter for the
// default. Mirrors tui.py's quick_confirm. EOF returns the default.
func (c *CLI) QuickConfirm(prompt string, def bool) bool {
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	prefix := "\n  ❯ " + prompt + " " + hint + ": "
	if !c.NoColor {
		prefix = "\n  " + ColorYellow + "❯ " + ColorReset + prompt + " " + ColorDim + hint + ColorReset + ": "
	}
	fmt.Fprint(c.Out, prefix)
	key, err := ReadKey(bufio.NewReader(c.In))
	if err != nil {
		return def
	}
	switch key {
	case KeyEnter, KeySpace:
		return def
	case "y":
		c.write(c.paint(ColorGreen, "yes") + "\n")
		return true
	case "n", KeyEsc:
		c.write(c.paint(ColorRed, "no") + "\n")
		return false
	default:
		return def
	}
}

// QuickPause prints msg and waits for a single line of input, mirroring
// tui.py's quick_pause.
func (c *CLI) QuickPause(msg string) {
	if msg == "" {
		msg = "Tekan tombol apa saja untuk melanjutkan..."
	}
	line := "\n  ❯ " + msg
	if !c.NoColor {
		line = "\n  " + ColorDim + "❯ " + msg + ColorReset
	}
	fmt.Fprint(c.Out, line)
	_, _ = ReadKey(bufio.NewReader(c.In))
	fmt.Fprintln(c.Out)
}
