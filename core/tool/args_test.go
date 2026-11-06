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
