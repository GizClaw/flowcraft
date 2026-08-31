//go:build windows

package windows

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"
)

// utf16BlockEntries decodes a UTF-16, double-NUL-terminated block (as
// used by CreateProcess) into its entries.
func utf16BlockEntries(p *uint16) []string {
	var entries []string
	cur := make([]uint16, 0, 32)
	for {
		c := *p
		p = (*uint16)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + unsafe.Sizeof(uint16(0))))
		if c != 0 {
			cur = append(cur, c)
			continue
		}
		if len(cur) == 0 {
			return entries
		}
		entries = append(entries, string(utf16.Decode(cur)))
		cur = cur[:0]
	}
}

func TestBuildEnvBlock(t *testing.T) {
	block := buildEnvBlock([]string{"PATH=C:\\bin", "FOO=1"})
	got := utf16BlockEntries(block)
	want := []string{"PATH=C:\\bin", "FOO=1"}
	if len(got) != len(want) {
		t.Fatalf("entries = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entries = %q, want %q", got, want)
		}
	}
}

func TestBuildEnvBlockEmpty(t *testing.T) {
	block := buildEnvBlock(nil)
	if got := utf16BlockEntries(block); len(got) != 0 {
		t.Fatalf("empty block decoded to %q, want no entries", got)
	}
}

func TestDedupEnvCase(t *testing.T) {
	got := dedupEnvCase([]string{
		"Path=C:\\first",
		"PATH=C:\\second",
		"FOO=1",
	})
	want := []string{"PATH=C:\\second", "FOO=1"}
	if len(got) != len(want) {
		t.Fatalf("dedup = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedup = %q, want %q", got, want)
		}
	}
}

func TestTTYExitError(t *testing.T) {
	e := &ttyExitError{code: 7}
	if e.ExitCode() != 7 {
		t.Fatalf("ExitCode() = %d, want 7", e.ExitCode())
	}
	if !strings.Contains(e.Error(), "7") {
		t.Fatalf("Error() = %q, want exit code", e.Error())
	}
}
