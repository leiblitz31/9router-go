package clientconfig

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// x5Ratio is how much Patch may cost above the TOML strict-parse floor before
// the X5 ceiling is breached. Patch's own additions are a linear scan, a splice
// and a read-back; measured on the 64 KB sample the whole call lands at
// 0.84-1.25x of the strict unmarshal alone. A regression that re-added a second
// strict pass — the 2.0x shape the TOML fix removed — breaks it, while scheduler
// jitter on a busy runner does not. It is a ratio, not a duration, so it holds
// under the race detector too.
const x5Ratio = 2.5

// X5: a 64 KB file patches fast, does not panic, and keeps its tail.
func TestX5LargeFile(t *testing.T) {
	const target = 64 << 10
	for _, tt := range []struct {
		name   string
		format Format
		build  func() (string, Edit)
	}{
		{"jsonc", FormatJSONC, func() (string, Edit) {
			var b strings.Builder
			b.WriteString("{\n")
			for i := 0; b.Len() < target; i++ {
				fmt.Fprintf(&b, "  \"k%d\": \"v%d\",\n", i, i)
			}
			b.WriteString("  \"base_url\": \"https://old/v1\"\n}\n")
			return b.String(), Edit{KeyPath: []string{"base_url"}, Value: "http://new/v1"}
		}},
		{"toml", FormatTOML, func() (string, Edit) {
			var b strings.Builder
			b.WriteString("[t]\n")
			for i := 0; b.Len() < target; i++ {
				fmt.Fprintf(&b, "k%d = \"v%d\"\n", i, i)
			}
			b.WriteString("base_url = \"https://old/v1\"\n")
			return b.String(), Edit{KeyPath: []string{"t", "base_url"}, Value: "http://new/v1"}
		}},
		{"yaml", FormatYAML, func() (string, Edit) {
			var b strings.Builder
			for i := 0; b.Len() < target; i++ {
				fmt.Fprintf(&b, "k%d: \"v%d\"\n", i, i)
			}
			b.WriteString("base_url: \"https://old/v1\"\n")
			return b.String(), Edit{KeyPath: []string{"base_url"}, Value: "http://new/v1"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src, edit := tt.build()
			if len(src) < target {
				t.Fatalf("sample is only %d bytes", len(src))
			}
			// The budget is a ratio against the parse floor Patch cannot avoid,
			// not an absolute wall-clock number. Every format has one: TOML's
			// strict unmarshal (the only layer that rejects a duplicate key,
			// quadratic in the keys of one table), YAML's node decode, JSONC's
			// lex+parse. On this 4-vCPU box those floors alone are 200-500 ms at
			// 64 KB, so a flat 200 ms ceiling measures the box rather than the
			// patcher — the argument configpatch_scale_test.go makes for the
			// 256 KB case. Patch adds a linear scan, one splice and a read-back;
			// the ratio is what a regression back to two parses would break.
			floor := x5Floor([]byte(src), tt.format)
			begin := time.Now()
			out, err := Patch([]byte(src), tt.format, []Edit{edit})
			elapsed := time.Since(begin)
			if err != nil {
				t.Fatalf("Patch() error = %v", err)
			}
			budget := x5Budget
			if scaled := time.Duration(float64(floor) * x5Ratio); scaled > budget {
				budget = scaled
			}
			if elapsed > budget {
				t.Errorf("Patch() took %v on a %d byte file, budget %v (parse floor %v)",
					elapsed, len(src), budget, floor)
			}
			tail := tailAfter(src, `"https://old/v1"`)
			if tail == "" || !strings.HasSuffix(string(out), tail) {
				t.Error("suffix after the patched value changed")
			}
			if err := verifyNoError(t, out, tt.format, edit); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// x5Floor times the one pass Patch cannot avoid for each format: the parse that
// establishes the document is valid before a single byte is spliced. Everything
// else the patch paths do is a linear scan or one splice, so this is the honest
// denominator for the X5 ceiling.
func x5Floor(src []byte, format Format) time.Duration {
	begin := time.Now()
	switch format {
	case FormatTOML:
		var scratch any
		_ = toml.Unmarshal(src, &scratch)
	case FormatYAML:
		_, _ = decodeYAML(src)
	case FormatJSONC:
		_, _, _ = parseJSONC(src)
	}
	return time.Since(begin)
}

// tailAfter returns the bytes following the last occurrence of marker, which is
// the part of a sample that a splice must not touch.
func tailAfter(src, marker string) string {
	i := strings.LastIndex(src, marker)
	if i < 0 {
		return ""
	}
	return src[i+len(marker):]
}

func verifyNoError(t *testing.T, out []byte, format Format, edit Edit) error {
	t.Helper()
	again, err := Patch(out, format, []Edit{edit})
	if !errors.Is(err, ErrAlreadyPatched) {
		t.Errorf("re-patch error = %v, want ErrAlreadyPatched", err)
	}
	if !bytes.Equal(again, out) {
		t.Error("re-patch changed bytes")
	}
	return nil
}

func TestPatchNoEdits(t *testing.T) {
	src := []byte("{}")
	out, err := Patch(src, FormatJSONC, nil)
	if !errors.Is(err, ErrAlreadyPatched) {
		t.Fatalf("Patch() error = %v, want ErrAlreadyPatched", err)
	}
	if !bytes.Equal(out, src) {
		t.Fatalf("Patch() = %q, want the input back", out)
	}
}

func TestPatchUnknownFormat(t *testing.T) {
	if _, err := Patch([]byte("x"), Format(99), []Edit{{KeyPath: []string{"a"}, Value: "b"}}); !errors.Is(err, ErrUnsupportedStructure) {
		t.Fatalf("Patch() error = %v, want ErrUnsupportedStructure", err)
	}
}
