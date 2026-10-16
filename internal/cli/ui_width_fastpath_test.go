package cli

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// Keep the pre-optimization width algorithm as an independent oracle: in
// particular ASCII controls historically take one column, and a selector
// only widens a non-ASCII character immediately before it.
func referenceTerminalWidth(text string, wrap int) (visible, rows, column int) {
	text = regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(text, "")
	rows = 1
	for text != "" {
		r, size := utf8.DecodeRuneInString(text)
		rest := text[size:]
		w := 1
		if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) {
			w = 0
		} else if kind := width.LookupRune(r).Kind(); kind == width.EastAsianWide || kind == width.EastAsianFullwidth {
			w = 2
		}
		if w == 1 && r > unicode.MaxASCII {
			if next, _ := utf8.DecodeRuneInString(rest); next == emojiSelector {
				w = 2
			}
		}
		visible += w
		if wrap > 0 && w > 0 && column > 0 && column+w > wrap {
			rows++
			column = 0
		}
		column += w
		text = rest
	}
	return visible, rows, column
}

func TestTerminalWidthFastPathsPreserveReference(t *testing.T) {
	t.Parallel()
	texts := []string{
		"", "plain table row with spaces 0123456789", "é", "项目", "❤️", "1️",
		"❤́️", "👩\u200d💻", "🇺🇸", "\xff❤\xfe", "\x1b[31mcolored 项目❤️\x1b[0m",
		"\x1b[m", "\x1b[1;32m", "\x1b[", "\x1b[31X", "\x1b[?25l", "x\x1by", "\x1b[31m\x1b[0m",
	}
	// Exercise every ASCII byte, including NUL, tab, newline, ESC and DEL,
	// both alone and next to the selector that must not widen ASCII.
	for r := rune(0); r <= unicode.MaxASCII; r++ {
		texts = append(texts, string(r), "a"+string(r)+"️项目")
	}
	for _, text := range texts {
		wantWidth, _, _ := referenceTerminalWidth(text, 0)
		if got := visibleWidth(text); got != wantWidth {
			t.Errorf("visibleWidth(%q) = %d, want %d", text, got, wantWidth)
		}
		for _, wrap := range []int{-1, 0, 1, 2, 3, 8, 40, 200} {
			_, wantRows, wantColumn := referenceTerminalWidth(text, wrap)
			if rows, column := lineMetrics(text, wrap); rows != wantRows || column != wantColumn {
				t.Errorf("lineMetrics(%q, %d) = (%d, %d), want (%d, %d)", text, wrap, rows, column, wantRows, wantColumn)
			}
		}
	}
}

// This budget protects the measured source of avoidable allocation: plain
// terminal measurement should not allocate a replacement copy of its input.
func TestPlainTerminalMeasurementDoesNotAllocate(t *testing.T) {
	text := strings.Repeat("Session number 12 · 项目 ", 8)
	if got := testing.AllocsPerRun(100, func() {
		_ = visibleWidth(text)
		_, _ = lineMetrics(text, 80)
	}); got != 0 {
		t.Fatalf("plain terminal measurement allocated %g times, want zero", got)
	}
}

func BenchmarkTerminalMeasurement(b *testing.B) {
	for _, color := range []bool{false, true} {
		text := strings.Repeat("Session number 12 · 项目 ❤️ ", 8)
		if color {
			text = "\x1b[32m" + text + "\x1b[0m"
		}
		b.Run(fmt.Sprintf("color=%t", color), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = visibleWidth(text)
				_, _ = lineMetrics(text, 80)
			}
		})
	}
}
