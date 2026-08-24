package sandbox

import (
	"sort"
	"strings"
	"unicode/utf16"
)

// quoteWinArg quotes one Windows command-line argument following the
// rules used by CommandLineToArgvW / the CRT, so spaces, quotes, and
// backslashes round-trip. The algorithm matches codex-rs's
// quote_windows_arg and Rust std::process::Command on Windows.
func quoteWinArg(arg string) string {
	needsQuotes := arg == "" ||
		strings.ContainsAny(arg, " \t\n\r\"")
	if !needsQuotes {
		return arg
	}

	var b strings.Builder
	b.Grow(len(arg) + 2)
	b.WriteByte('"')
	backslashes := 0
	for _, ch := range arg {
		switch ch {
		case '\\':
			backslashes++
		case '"':
			b.WriteString(strings.Repeat("\\", backslashes*2+1))
			b.WriteByte('"')
			backslashes = 0
		default:
			if backslashes > 0 {
				b.WriteString(strings.Repeat("\\", backslashes))
				backslashes = 0
			}
			b.WriteRune(ch)
		}
	}
	if backslashes > 0 {
		b.WriteString(strings.Repeat("\\", backslashes*2))
	}
	b.WriteByte('"')
	return b.String()
}

// argvToCommandLine builds the command line passed to
// CreateProcessAsUserW, quoting every argument independently.
func argvToCommandLine(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, quoteWinArg(arg))
	}
	return strings.Join(quoted, " ")
}

// makeEnvBlock renders a []string env ("K=V" entries) into the UTF-16
// double-null-terminated environment block CreateProcessAsUserW
// expects, sorted case-insensitively by key (the Windows convention).
// The result always ends with the final terminator, so it is safe to
// pass even when env is empty.
func makeEnvBlock(env []string) []uint16 {
	sorted := append([]string(nil), env...)
	sort.Slice(sorted, func(i, j int) bool {
		ik := sorted[i]
		jk := sorted[j]
		if eq := strings.IndexByte(ik, '='); eq >= 0 {
			ik = ik[:eq]
		}
		if eq := strings.IndexByte(jk, '='); eq >= 0 {
			jk = jk[:eq]
		}
		li := strings.ToLower(ik)
		lj := strings.ToLower(jk)
		if li != lj {
			return li < lj
		}
		return ik < jk
	})

	var block []uint16
	for _, kv := range sorted {
		u := utf16.Encode([]rune(kv))
		block = append(block, u...)
		block = append(block, 0)
	}
	block = append(block, 0)
	return block
}
