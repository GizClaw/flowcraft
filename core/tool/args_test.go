package tool

import (
	"strings"
	"testing"
)

type execArgs struct {
	Command        string   `json:"command"`
	Workdir        string   `json:"workdir"`
	TimeoutSeconds *float64 `json:"timeout_seconds"`
}

var certAliases = map[string]string{"cmd": "command"}

func TestDecodeAcceptsAlias(t *testing.T) {
	var args execArgs
	if err := DecodeArgs("exec_command", `{"cmd":"go build ./..."}`, certAliases, &args); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if args.Command != "go build ./..." {
		t.Fatalf("command = %q", args.Command)
	}
}

func TestDecodeCanonicalKeyWinsOverAlias(t *testing.T) {
	var args execArgs
	if err := DecodeArgs("exec_command",
		`{"command":"canonical","cmd":"alias"}`, certAliases, &args); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if args.Command != "canonical" {
		t.Fatalf("command = %q, want the canonical key to win", args.Command)
	}
}

// TestDecodeNamesUnknownKey: the old loose decode dropped "cwd" and
// answered "command is required"; the error must name the key the
// model actually sent, plus what the tool accepts.
func TestDecodeNamesUnknownKey(t *testing.T) {
	var args execArgs
	err := DecodeArgs("exec_command", `{"cwd":"/tmp","command":"ls"}`, certAliases, &args)
	if err == nil {
		t.Fatal("unknown key accepted")
	}
	msg := err.Error()
	for _, want := range []string{
		`exec_command: unknown argument "cwd"`,
		`accepted arguments: command (alias: cmd)`,
		"timeout_seconds",
		"workdir",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func TestDecodeReportsEveryUnknownKey(t *testing.T) {
	var args execArgs
	err := DecodeArgs("exec_command", `{"cwd":"/tmp","shell":"zsh"}`, certAliases, &args)
	if err == nil {
		t.Fatal("unknown keys accepted")
	}
	if msg := err.Error(); !strings.Contains(msg, "unknown arguments") ||
		!strings.Contains(msg, `"cwd"`) || !strings.Contains(msg, `"shell"`) {
		t.Fatalf("error = %q, want both keys named", msg)
	}
}

func TestDecodeTypeErrorNamesTheTool(t *testing.T) {
	var args execArgs
	err := DecodeArgs("exec_command", `{"command":"ls","timeout_seconds":"soon"}`, nil, &args)
	if err == nil {
		t.Fatal("bad type accepted")
	}
	if msg := err.Error(); !strings.Contains(msg, "exec_command: parse arguments:") {
		t.Fatalf("error = %q", msg)
	}
}

func TestDecodeRejectsNonObjectShapes(t *testing.T) {
	for _, arguments := range []string{``, `   `, `"ls"`, `[1,2]`, `null`} {
		var args execArgs
		err := DecodeArgs("exec_command", arguments, nil, &args)
		if err == nil {
			t.Fatalf("DecodeArgs(%q) unexpectedly succeeded", arguments)
		}
		if !strings.Contains(err.Error(), "exec_command: parse arguments:") {
			t.Errorf("DecodeArgs(%q) error = %v", arguments, err)
		}
	}
}

func TestDecodeRejectsNonStructTarget(t *testing.T) {
	var notAStruct string
	if err := DecodeArgs("exec_command", `{}`, nil, &notAStruct); err == nil {
		t.Fatal("non-struct target accepted")
	}
}

// TestDecodeArgsAllocations guards the single-pass path. The number is the wire
// cost of decoding one argument object into a struct: two more passes over the
// payload plus the accepted-key reflection is what the diagnostic path pays, and
// it must stay off the happy path. The cap leaves room for a json decoder's
// internal growth without masking a regression back to three passes.
func TestDecodeArgsAllocations(t *testing.T) {
	arguments := `{"path":"/tmp/x.txt","content":"hello","offset":10,"limit":100}`
	var out struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
	}
	if err := DecodeArgs("bench", arguments, nil, &out); err != nil {
		t.Fatalf("DecodeArgs: %v", err)
	}
	allocs := testing.AllocsPerRun(1000, func() {
		var target struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Offset  int    `json:"offset"`
			Limit   int    `json:"limit"`
		}
		if err := DecodeArgs("bench", arguments, nil, &target); err != nil {
			t.Fatalf("DecodeArgs: %v", err)
		}
	})
	if allocs > 10 {
		t.Fatalf("DecodeArgs allocates %.0f times per call, want <= 10", allocs)
	}
}

// TestDecodeAcceptsPaddedObject: models emit leading whitespace around the
// argument object, and the fast path must not treat that as a non-object.
func TestDecodeAcceptsPaddedObject(t *testing.T) {
	var args execArgs
	if err := DecodeArgs("exec_command", "\n\t {\"command\":\"ls\"}\n", nil, &args); err != nil {
		t.Fatalf("DecodeArgs: %v", err)
	}
	if args.Command != "ls" {
		t.Fatalf("command = %q", args.Command)
	}
}

// TestDecodeArgsFastPathMatchesSlowPath is the equivalence guard for the
// shortcut in DecodeArgs. The fast path and the diagnostic path must agree on
// what is acceptable — not just on the happy payload, but on the shapes where
// the primitives they use differ. Trailing data is the one that bites:
// Decoder.Decode reads a single value and stops, while the old first pass
// (json.Unmarshal over the whole payload) refused anything after it. A nil alias
// map versus a non-nil one is exactly how the two paths are selected, so the
// same payload is decoded both ways.
//
// The one documented exception is a case-variant key, asserted separately in
// TestDecodeArgsKeyCaseIsInsensitiveOnTheFastPath.
func TestDecodeArgsFastPathMatchesSlowPath(t *testing.T) {
	for _, payload := range []string{
		`{"command":"ls"}`,
		`{"command":"ls"}junk`,
		`{"command":"ls"}{"command":"rm"}`,
		`{"command":"ls"} `,
		`{"command":"ls","timeout_seconds":5}`,
		`{"command":"ls","timeout_seconds":"soon"}`,
		`{"command":null}`,
		`{}`,
		`{"cwd":"/tmp","command":"ls"}`,
	} {
		var fast, slow execArgs
		fastErr := DecodeArgs("exec_command", payload, nil, &fast)
		slowErr := DecodeArgs("exec_command", payload, certAliases, &slow)
		if (fastErr == nil) != (slowErr == nil) {
			t.Errorf("%q: fast err = %v, slow err = %v", payload, fastErr, slowErr)
			continue
		}
		if fastErr != nil {
			continue // both refused; the message is the slow path's own concern
		}
		if fast.Command != slow.Command || fast.Workdir != slow.Workdir {
			t.Errorf("%q: fast decoded %+v, slow decoded %+v", payload, fast, slow)
		}
		if (fast.TimeoutSeconds == nil) != (slow.TimeoutSeconds == nil) {
			t.Errorf("%q: fast timeout = %v, slow timeout = %v", payload, fast.TimeoutSeconds, slow.TimeoutSeconds)
		}
		if fast.TimeoutSeconds != nil && *fast.TimeoutSeconds != *slow.TimeoutSeconds {
			t.Errorf("%q: fast timeout = %v, slow timeout = %v", payload, *fast.TimeoutSeconds, *slow.TimeoutSeconds)
		}
	}
}

// TestDecodeArgsKeyCaseIsInsensitiveOnTheFastPath records a deliberate
// difference between the two paths. The fast path decodes with
// DisallowUnknownFields, whose field matching is case-insensitive (the
// encoding/json rule), while the diagnostic path compares the caller's raw keys
// against the json tags literally. So {"Command":"ls"} is accepted by a tool
// without aliases and refused by one with them.
//
// It is documented rather than eliminated because eliminating it means building
// the accepted-key set on the hot path — exactly the reflection this change
// removed. The accepted key still lands in the right field, so the tool does
// what the model asked; the cost is a friendlier validator, not a wrong one.
func TestDecodeArgsKeyCaseIsInsensitiveOnTheFastPath(t *testing.T) {
	var fast execArgs
	if err := DecodeArgs("exec_command", `{"Command":"ls"}`, nil, &fast); err != nil {
		t.Fatalf("fast path rejected a case-variant key: %v", err)
	}
	if fast.Command != "ls" {
		t.Fatalf("command = %q, want the value to land in the field", fast.Command)
	}
	var slow execArgs
	if err := DecodeArgs("exec_command", `{"Command":"ls"}`, certAliases, &slow); err == nil {
		t.Fatal("diagnostic path accepted a case-variant key; the documented difference is gone")
	}
}
