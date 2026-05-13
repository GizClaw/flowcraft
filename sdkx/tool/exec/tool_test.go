package exec_test

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdkx/tool/exec"
)

// fakeRunner is a sandbox.Runner stub that captures the most recent
// Exec call and returns canned output, so tests can prove the tool
// translates args → sandbox.ExecOptions correctly without spawning
// real processes. Use the real LocalRunner only for E2E-style
// happy-path coverage at the end of this file.
type fakeRunner struct {
	gotCmd  string
	gotArgs []string
	gotOpts sandbox.ExecOptions

	retResult *sandbox.ExecResult
	retErr    error
}

func (f *fakeRunner) Exec(_ context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	f.gotCmd = cmd
	f.gotArgs = args
	f.gotOpts = opts
	if f.retErr != nil {
		return nil, f.retErr
	}
	if f.retResult != nil {
		return f.retResult, nil
	}
	return &sandbox.ExecResult{}, nil
}

func TestNew_NilRunner_Rejected(t *testing.T) {
	_, err := exec.New(nil)
	if err == nil {
		t.Fatal("New with nil runner should be rejected (deny-by-default)")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected errdefs.IsValidation, got: %v", err)
	}
}

func TestMustNew_NilRunner_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustNew with nil runner should panic")
		}
	}()
	_ = exec.MustNew(nil)
}

func TestNew_NoopRunner_OK(t *testing.T) {
	// The explicit-noop bypass is the documented escape hatch from
	// deny-by-default; make sure it actually compiles + constructs.
	tl, err := exec.New(sandbox.NoopRunner{})
	if err != nil {
		t.Fatalf("New(NoopRunner) should succeed: %v", err)
	}
	if tl == nil {
		t.Fatal("New returned nil tool")
	}
}

func TestDefinition_Shape(t *testing.T) {
	tl, err := exec.New(sandbox.NoopRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	def := tl.Definition()
	if def.Name != exec.Name {
		t.Fatalf("Definition.Name = %q, want %q", def.Name, exec.Name)
	}
	// Schema must require command — without it the LLM gets no
	// hint that command is mandatory and we lose the validation
	// fast-path.
	schema := def.InputSchema
	if schema == nil {
		t.Fatal("InputSchema should not be nil")
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "command" {
		t.Fatalf("required = %v, want [command]", required)
	}
}

func TestExecute_HappyPath_ForwardsArgs(t *testing.T) {
	rn := &fakeRunner{retResult: &sandbox.ExecResult{ExitCode: 0, Stdout: "hello\n", Stderr: ""}}
	tl, err := exec.New(rn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := tl.Execute(context.Background(), `{"command":"echo","args":["hello"]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verify the JSON result shape — exit_code is the structured
	// field the model needs to distinguish "ran but failed" from
	// "ran and succeeded".
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("result not valid JSON: %v (out=%q)", err, out)
	}
	if got["exit_code"] != float64(0) {
		t.Fatalf("exit_code = %v, want 0", got["exit_code"])
	}
	if got["stdout"] != "hello\n" {
		t.Fatalf("stdout = %v, want 'hello\\n'", got["stdout"])
	}

	// Verify the args reached the runner unchanged.
	if rn.gotCmd != "echo" {
		t.Fatalf("runner saw cmd %q, want echo", rn.gotCmd)
	}
	if len(rn.gotArgs) != 1 || rn.gotArgs[0] != "hello" {
		t.Fatalf("runner saw args %v, want [hello]", rn.gotArgs)
	}
}

func TestExecute_NonZeroExit_NotAnError(t *testing.T) {
	// Sandboxed sub-processes exit non-zero for legitimate reasons
	// (test failures, grep miss, ...) — those must NOT bubble up
	// as a Go error to the LLM round, otherwise the model can't
	// reason about exit codes and the round will look like a
	// tool-system failure to the agent harness.
	rn := &fakeRunner{retResult: &sandbox.ExecResult{ExitCode: 1, Stdout: "", Stderr: "boom"}}
	tl, _ := exec.New(rn)

	out, err := tl.Execute(context.Background(), `{"command":"false"}`)
	if err != nil {
		t.Fatalf("non-zero exit must not be a Go error, got: %v", err)
	}
	var r map[string]any
	_ = json.Unmarshal([]byte(out), &r)
	if r["exit_code"] != float64(1) {
		t.Fatalf("exit_code = %v, want 1", r["exit_code"])
	}
	if r["stderr"] != "boom" {
		t.Fatalf("stderr = %v, want 'boom'", r["stderr"])
	}
}

func TestExecute_BadJSON_Validation(t *testing.T) {
	tl, _ := exec.New(sandbox.NoopRunner{})

	_, err := tl.Execute(context.Background(), `{not json`)
	if err == nil {
		t.Fatal("malformed JSON should be rejected")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected errdefs.IsValidation, got: %v", err)
	}
}

func TestExecute_EmptyCommand_Validation(t *testing.T) {
	tl, _ := exec.New(sandbox.NoopRunner{})

	for _, raw := range []string{
		`{}`,
		`{"command":""}`,
		`{"command":"   "}`,
	} {
		_, err := tl.Execute(context.Background(), raw)
		if err == nil {
			t.Fatalf("empty/whitespace command must be rejected (input=%q)", raw)
		}
		if !errdefs.IsValidation(err) {
			t.Fatalf("input=%q: expected errdefs.IsValidation, got: %v", raw, err)
		}
	}
}

func TestExecute_SandboxError_Forwarded(t *testing.T) {
	// Sandbox-side errors (NotAvailable / Forbidden / Timeout /
	// path-traversal) MUST be forwarded verbatim so callers can
	// classify them with errdefs.Is*. The tool layer adds nothing
	// of value to those errors — the sandbox already knows why it
	// refused.
	want := errdefs.NotAvailablef("sandbox: net policy not supported")
	rn := &fakeRunner{retErr: want}
	tl, _ := exec.New(rn)

	_, err := tl.Execute(context.Background(), `{"command":"echo"}`)
	if err == nil {
		t.Fatal("sandbox error should not be swallowed")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("expected errdefs.IsNotAvailable, got: %v", err)
	}
	if !errors.Is(err, want) {
		t.Fatalf("err chain should preserve sandbox sentinel: %v", err)
	}
}

func TestExecute_Stdin_PassedThrough(t *testing.T) {
	rn := &fakeRunner{retResult: &sandbox.ExecResult{}}
	tl, _ := exec.New(rn)

	_, err := tl.Execute(context.Background(), `{"command":"cat","stdin":"piped input"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(rn.gotOpts.Stdin) != "piped input" {
		t.Fatalf("stdin not forwarded: got %q", rn.gotOpts.Stdin)
	}
}

func TestExecute_Workdir_PassedThrough(t *testing.T) {
	rn := &fakeRunner{retResult: &sandbox.ExecResult{}}
	tl, _ := exec.New(rn)

	_, err := tl.Execute(context.Background(), `{"command":"ls","workdir":"subdir"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rn.gotOpts.WorkDir != "subdir" {
		t.Fatalf("workdir not forwarded: got %q", rn.gotOpts.WorkDir)
	}
}

func TestExecute_Timeout_Precedence(t *testing.T) {
	// per-call > tool-default > zero. Cover all three layers so a
	// future refactor cannot silently reorder them.
	tests := []struct {
		name     string
		toolOpt  []exec.Option
		argsJSON string
		want     time.Duration
	}{
		{
			name:     "per-call wins over default",
			toolOpt:  []exec.Option{exec.WithDefaultTimeout(5 * time.Second)},
			argsJSON: `{"command":"echo","timeout_seconds":2}`,
			want:     2 * time.Second,
		},
		{
			name:     "default applies when omitted",
			toolOpt:  []exec.Option{exec.WithDefaultTimeout(7 * time.Second)},
			argsJSON: `{"command":"echo"}`,
			want:     7 * time.Second,
		},
		{
			name:     "zero per-call disables tool timeout",
			toolOpt:  []exec.Option{exec.WithDefaultTimeout(5 * time.Second)},
			argsJSON: `{"command":"echo","timeout_seconds":0}`,
			want:     0,
		},
		{
			name:     "no defaults, no per-call",
			argsJSON: `{"command":"echo"}`,
			want:     0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rn := &fakeRunner{retResult: &sandbox.ExecResult{}}
			tl, err := exec.New(rn, tc.toolOpt...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := tl.Execute(context.Background(), tc.argsJSON); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if rn.gotOpts.Timeout != tc.want {
				t.Fatalf("Timeout = %v, want %v", rn.gotOpts.Timeout, tc.want)
			}
		})
	}
}

func TestExecute_E2E_LocalRunner(t *testing.T) {
	// One end-to-end test against the real LocalRunner so the
	// JSON-args → sandbox.ExecOptions → os/exec chain is exercised
	// at least once. Substantive sandbox coverage lives in
	// sdk/sandbox; we just confirm wiring here.
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	rn := sandbox.NewLocalRunner(t.TempDir())
	tl, err := exec.New(rn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := tl.Execute(context.Background(), `{"command":"echo","args":["world"]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	if r["exit_code"] != float64(0) {
		t.Fatalf("exit_code = %v, want 0", r["exit_code"])
	}
	if s, _ := r["stdout"].(string); !strings.Contains(s, "world") {
		t.Fatalf("stdout = %q, want substring 'world'", s)
	}
}
