package sandbox

import (
	"strings"
	"testing"
)

func TestQuoteWinArg(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"", `""`},
		{"has space", `"has space"`},
		// Backslashes alone never trigger quoting (CommandLineToArgvW
		// rule); they only matter inside quotes.
		{`trailing\`, `trailing\`},
		{`back\slash`, `back\slash`},
		{`has\ space`, `"has\ space"`},
		{`trailing\ `, `"trailing\ "`},
		{`quote"inside`, `"quote\"inside"`},
		{`a"b\`, `"a\"b\\"`},
		{"tab\there", "\"tab\there\""},
	}
	for _, tt := range tests {
		if got := quoteWinArg(tt.in); got != tt.want {
			t.Errorf("quoteWinArg(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestArgvToCommandLine(t *testing.T) {
	got := argvToCommandLine([]string{`C:\Program Files\git\git.exe`, "status", `a"b`})
	want := `"C:\Program Files\git\git.exe" status "a\"b"`
	if got != want {
		t.Errorf("argvToCommandLine = %q, want %q", got, want)
	}
}

func TestMakeEnvBlock(t *testing.T) {
	block := makeEnvBlock([]string{"PATH=C:\\bin", "A=1", "a=2"})
	// Entries are sorted case-insensitively: A=1, a=2, PATH=...
	var entries []string
	cur := strings.Builder{}
	for _, u := range block {
		if u == 0 {
			if cur.Len() > 0 {
				entries = append(entries, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteRune(rune(u))
	}
	want := []string{"A=1", "a=2", "PATH=C:\\bin"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %v, want %v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("entries = %v, want %v", entries, want)
		}
	}
	// Double-null terminator: two trailing zero words.
	if block[len(block)-1] != 0 || block[len(block)-2] != 0 {
		t.Fatal("environment block must end with a double null terminator")
	}
}

func TestMakeEnvBlockEmpty(t *testing.T) {
	block := makeEnvBlock(nil)
	// An empty block is the single block terminator (entry list is
	// empty, plus the trailing block terminator), matching codex-rs.
	if len(block) != 1 || block[0] != 0 {
		t.Fatalf("empty env block = %v, want single null", block)
	}
}
