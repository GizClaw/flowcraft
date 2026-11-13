package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox/local"
	"github.com/goccy/go-json"
)

func newTool(t *testing.T) *CommandTool {
	t.Helper()
	runner := local.New(t.TempDir())
	t.Cleanup(func() { _ = runner.Close() })
	command, err := NewCommand(runner)
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	return command
}

func run(t *testing.T, command *CommandTool, arguments map[string]any) (map[string]any, error) {
	t.Helper()
	payload, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	content, err := command.Execute(context.Background(), string(payload))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(content.Text()), &out); err != nil {
		t.Fatalf("decode result %q: %v", content.Text(), err)
	}
	return out, nil
}

// A plain command runs directly: the spawn receives the program and its
// arguments, with no shell interpreting them.
func TestDirectCommand(t *testing.T) {
	out, err := run(t, newTool(t), map[string]any{"command": "/bin/echo hello world"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out["exit_code"].(float64) != 0 {
		t.Fatalf("exit = %v, stderr = %v", out["exit_code"], out["stderr"])
	}
	if strings.TrimSpace(out["output"].(string)) != "hello world" {
		t.Errorf("stdout = %q", out["stdout"])
	}
}

// Shell syntax goes through the platform shell; the model's line is the command.
func TestShellCommand(t *testing.T) {
	out, err := run(t, newTool(t), map[string]any{"command": "printf 'a\\n' | tr a-z A-Z"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out["exit_code"].(float64) != 0 {
		t.Fatalf("exit = %v, stderr = %v", out["exit_code"], out["stderr"])
	}
	if strings.TrimSpace(out["output"].(string)) != "A" {
		t.Errorf("stdout = %q, want A", out["stdout"])
	}
}

// A non-zero exit is a result, not a tool error: the model needs the code.
func TestNonZeroExitIsAResult(t *testing.T) {
	out, err := run(t, newTool(t), map[string]any{"command": "/bin/sh -c 'exit 3'"})
	if err != nil {
		t.Fatalf("exec returned an error for a non-zero exit: %v", err)
	}
	if out["exit_code"].(float64) != 3 {
		t.Errorf("exit = %v, want 3", out["exit_code"])
	}
}

// A command that writes lands where the sandbox root is, and the tool reports
// success rather than the file.
func TestCommandWritesInWorkdir(t *testing.T) {
	dir := t.TempDir()
	runner := local.New(dir)
	t.Cleanup(func() { _ = runner.Close() })
	command, err := NewCommand(runner)
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	out, err := run(t, command, map[string]any{"command": "/bin/sh -c 'echo hi > out.txt'"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out["exit_code"].(float64) != 0 {
		t.Fatalf("exit = %v, stderr = %v", out["exit_code"], out["stderr"])
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "hi" {
		t.Fatalf("written file = %q, %v", data, err)
	}
}

func TestEmptyCommandRejected(t *testing.T) {
	if _, err := run(t, newTool(t), map[string]any{"command": "   "}); err == nil {
		t.Fatal("empty command accepted")
	}
}

func TestMetadataDeclaresMutation(t *testing.T) {
	if !newTool(t).Metadata().MutatesState {
		t.Error("MutatesState = false, want true")
	}
}

// Output beyond the tool's bound is cut, and the result says so: a model that
// cannot tell a short answer from a truncated one will ask again.
func TestOutputIsCappedAndReported(t *testing.T) {
	out, err := run(t, newTool(t), map[string]any{
		"command": "/bin/sh -c 'head -c 2000000 /dev/zero | tr \"\\\\0\" x'",
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out["truncated"] != true {
		t.Errorf("truncated = %v, want true", out["truncated"])
	}
	if got := len(out["output"].(string)); got != maxReturnedOutput {
		t.Errorf("output = %d bytes, want the %d inline cap", got, maxReturnedOutput)
	}
	if path, ok := out["full_output_path"].(string); !ok || path == "" {
		t.Errorf("truncated output gave no full_output_path: %v", out)
	}
}

// Truncation must not copy a stream that fits, and must cut on a byte boundary
// when it does not.
func TestTruncateOutput(t *testing.T) {
	short := "abc"
	if got, cut := truncateOutput(short, 10); cut || got != short {
		t.Errorf("truncateOutput(%q, 10) = %q, %v", short, got, cut)
	}
	long := strings.Repeat("x", 20)
	got, cut := truncateOutput(long, 5)
	if !cut || got != "xxxxx" {
		t.Errorf("truncateOutput = %q, %v; want %q, true", got, cut, "xxxxx")
	}
}

// Both streams land in the spool file, separated only when needed.
func TestSpoolWritesBothStreams(t *testing.T) {
	path, err := spool("out", "err")
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if string(data) != "out\nerr" {
		t.Errorf("spool = %q, want %q", data, "out\nerr")
	}
}
