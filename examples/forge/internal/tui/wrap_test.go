package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestWrapLineNeverSplitsRunes(t *testing.T) {
	text := "我1号林知，预言家。昨晚死的是4号小满。"
	lines := wrapLine(text, 10)
	joined := strings.Join(lines, "")
	if joined != text {
		t.Fatalf("wrap rejoined %q, want %q", joined, text)
	}
	for _, line := range lines {
		if !utf8Valid(line) {
			t.Fatalf("wrap produced invalid UTF-8: %q", line)
		}
		if w := runewidth.StringWidth(line); w > 10 {
			t.Fatalf("line width %d exceeds 10: %q", w, line)
		}
	}
}

func TestWrapLineAccountForWideRunes(t *testing.T) {
	// Each rune is 2 display columns: at width 4, "你好世界" must wrap
	// into exactly two lines ("你好" / "世界"), never split a rune.
	lines := wrapLine("你好世界", 4)
	if len(lines) != 2 {
		t.Fatalf("wrap count = %d, want 2 (%q)", len(lines), lines)
	}
	for _, line := range lines {
		if w := runewidth.StringWidth(line); w > 4 {
			t.Fatalf("line width %d exceeds 4: %q", w, line)
		}
	}
}

func TestWrapLineEmptyAndNarrow(t *testing.T) {
	if got := wrapLine("", 10); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty text = %q", got)
	}
	if got := wrapLine("hello", 0); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("width <= 0 must return text unchanged: %q", got)
	}
}

func utf8Valid(s string) bool {
	return !strings.ContainsRune(s, '\uFFFD')
}
