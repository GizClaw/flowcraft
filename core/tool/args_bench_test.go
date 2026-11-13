package tool

import (
	"strings"
	"testing"
)

// Both benchmarks measure the same work a builtin tool does before it touches
// the filesystem: turn a tool call's argument object into a typed struct. The
// payload sizes bracket the real cases — read_file/edit carry a path, write_file
// carries a whole file body.
type benchArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit"`
}

func benchArguments(payload int) string {
	var b strings.Builder
	b.Grow(payload + 96)
	b.WriteString(`{"path":"/tmp/x.txt","content":"`)
	for i := 0; i < payload; i++ {
		b.WriteByte('a')
	}
	b.WriteString(`","offset":10,"limit":100}`)
	return b.String()
}

// BenchmarkDecodeArgs is the guard for the dispatch fast path: the argument
// object is decoded once, and a diagnostic re-parse only happens on an error a
// model can fix.
func BenchmarkDecodeArgs(b *testing.B) {
	for _, size := range []struct {
		name    string
		payload int
	}{
		{"80B", 80},
		{"8KiB", 8 << 10},
		{"64KiB", 64 << 10},
	} {
		arguments := benchArguments(size.payload)
		b.Run(size.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(arguments)))
			for i := 0; i < b.N; i++ {
				var out benchArgs
				if err := DecodeArgs("bench", arguments, nil, &out); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkDecodeArgsAliased covers the one call site that maps a legacy key
// onto its canonical name (exec_command's "cmd"). It keeps the alias path
// visible so the fast path cannot quietly drop it.
func BenchmarkDecodeArgsAliased(b *testing.B) {
	arguments := `{"cmd":"ls -la","timeout":10}`
	type aliased struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	aliases := map[string]string{"cmd": "command"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var out aliased
		if err := DecodeArgs("bench", arguments, aliases, &out); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAcceptedFields measures the reflection pass. It should leave the hot
// path entirely: the accepted-key map only feeds an error message.
func BenchmarkAcceptedFields(b *testing.B) {
	var out benchArgs
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := acceptedFields(&out); err != nil {
			b.Fatal(err)
		}
	}
}
