package runtime

import (
	"context"
	"errors"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/sandbox"
	sandboxengine "github.com/GizClaw/flowcraft/core/sandbox/engine"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
	builtinexec "github.com/GizClaw/flowcraft/core/tool-builtin/exec"
)

// recordingRunner is what the test decorator returns. It counts which entry
// point the tools used and forwards everything to the composed chain, so a test
// can prove the wrapper is on the path instead of inferring it from behavior.
type recordingRunner struct {
	*sandboxengine.Forwarder

	execCalls  int
	startCalls int
	closeCalls int
}

func (r *recordingRunner) Exec(
	ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions,
) (*sandbox.ExecResult, error) {
	r.execCalls++
	return sandboxengine.Exec(ctx, r.Inner, cmd, args, opts)
}

func (r *recordingRunner) Start(
	ctx context.Context, spec sandbox.SessionSpec,
) (sandbox.Session, error) {
	r.startCalls++
	return r.Forwarder.Start(ctx, spec)
}

func (r *recordingRunner) Close() error {
	r.closeCalls++
	return r.Forwarder.Close()
}

// closeRecorder counts the Close that reaches the composed chain, which is what
// "the runtime closes the outermost value exactly once and that value forwards"
// means in practice.
type closeRecorder struct {
	*sandboxengine.Forwarder
	closeCalls int
}

func (c *closeRecorder) Close() error {
	c.closeCalls++
	return c.Forwarder.Close()
}

// recordingDecorator builds the nested chain the tests assert on: a recording
// wrapper over a close recorder over the composed runner.
func recordingDecorator(t *testing.T, outer **recordingRunner, inner **closeRecorder) Option {
	t.Helper()
	return WithSandboxDecorator(func(_ Sandbox, composed sandbox.Runner) (sandbox.Runner, error) {
		innerForwarder, err := sandboxengine.NewForwarder(composed)
		if err != nil {
			return nil, err
		}
		recorder := &closeRecorder{Forwarder: innerForwarder}
		outerForwarder, err := sandboxengine.NewForwarder(recorder)
		if err != nil {
			return nil, err
		}
		*inner = recorder
		*outer = &recordingRunner{Forwarder: outerForwarder}
		return *outer, nil
	})
}

// The decorator runs after the backend and after the document's policy, so the
// runner it receives already carries engine.WithDefaults, and the resolved view
// gives a host what it needs to build a companion chain. This part needs no
// command, so it runs on every platform, including the Windows lane.
func TestSandboxDecoratorResolvesTheDocument(t *testing.T) {
	workDir := t.TempDir()
	cfg := localConfig(t)
	cfg.Sandbox.WorkDir = workDir
	cfg.Sandbox.Timeout = "7s"

	var (
		seen     Sandbox
		composed sandbox.Runner
	)
	rt, err := New(t.Context(), cfg, WithSandboxDecorator(
		func(spec Sandbox, runner sandbox.Runner) (sandbox.Runner, error) {
			seen, composed = spec, runner
			return runner, nil
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if seen.WorkDir != workDir {
		t.Fatalf("WorkDir = %q, want sandbox.workdir %q", seen.WorkDir, workDir)
	}
	if seen.Defaults.Timeout != 7*time.Second {
		t.Fatalf("Defaults.Timeout = %v, want 7s", seen.Defaults.Timeout)
	}
	if composed == nil {
		t.Fatal("the decorator received no runner")
	}
}

// The runner the decorator receives is the composed chain, defaults included: a
// call that asks for nothing still hits the deployment's timeout ceiling. A host
// that wanted to wire a retry chain without this would rebuild the policy.
func TestSandboxDecoratorReceivesComposedRunner(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("uses a POSIX shell for the timeout probe")
	}
	cfg := localConfig(t)
	cfg.Sandbox.Timeout = "300ms"

	var composed sandbox.Runner
	rt, err := New(t.Context(), cfg, WithSandboxDecorator(
		func(_ Sandbox, runner sandbox.Runner) (sandbox.Runner, error) {
			composed = runner
			return runner, nil
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	start := time.Now()
	_, err = sandboxengine.Exec(t.Context(), composed,
		sandboxengine.DefaultShell(goruntime.GOOS).Path,
		[]string{"-c", "sleep 5"}, sandbox.ExecOptions{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("the composed runner ran a command the deployment's timeout should have cut off")
	}
	if !errdefs.IsTimeout(err) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the 300ms ceiling to surface as a timeout", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("elapsed = %v; the 300ms ceiling was not applied inside the decorator", elapsed)
	}
}

// Both tool paths reach the decorated runner: exec_command through Exec (the
// one-shot override) and exec_session through Start. Sessions stay on the
// confined chain by construction — the wrapper never forwards Start to a retry.
func TestSandboxDecoratorSeesBothToolPaths(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	var (
		recorder *recordingRunner
		inner    *closeRecorder
	)
	rt, err := New(t.Context(), localConfig(t), recordingDecorator(t, &recorder, &inner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	shell := sandboxengine.DefaultShell(goruntime.GOOS)
	call, err := message.NewToolCall("call-1", shell.Name, map[string]any{"command": "echo decorated"})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	result := rt.Assembly().Execute(rt.Context(t.Context()), call)
	if result.IsError {
		t.Fatalf("exec result = %q", result.Content.Text())
	}
	if recorder.execCalls != 1 || recorder.startCalls != 0 {
		t.Fatalf("exec calls = %d, start calls = %d; want 1 and 0",
			recorder.execCalls, recorder.startCalls)
	}
	if !strings.Contains(result.Content.Text(), "decorated") {
		t.Fatalf("exec output = %q, want the command's output", result.Content.Text())
	}

	start, err := message.NewToolCall("call-2", builtinexec.SessionName, map[string]any{
		"action": "start",
		"argv":   []string{"echo", "session"},
	})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	if result := rt.Assembly().Execute(rt.Context(t.Context()), start); result.IsError {
		t.Fatalf("session start = %q", result.Content.Text())
	}
	if recorder.startCalls != 1 || recorder.execCalls != 1 {
		t.Fatalf("start calls = %d, exec calls = %d; want 1 and 1",
			recorder.startCalls, recorder.execCalls)
	}
}

// The runtime is the sole owner of the value the decorator returns: it closes it
// once, that value forwards to the composed chain, and a second Close is a no-op.
func TestSandboxDecoratorIsClosedExactlyOnce(t *testing.T) {
	var (
		recorder *recordingRunner
		inner    *closeRecorder
	)
	rt, err := New(t.Context(), localConfig(t), recordingDecorator(t, &recorder, &inner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rt.Runner() != sandbox.Runner(recorder) {
		t.Fatalf("Runner() = %T, want the decorated value: tools and host must agree",
			rt.Runner())
	}

	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if recorder.closeCalls != 1 || inner.closeCalls != 1 {
		t.Fatalf("close calls: outer = %d, composed = %d; want 1 and 1",
			recorder.closeCalls, inner.closeCalls)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if recorder.closeCalls != 1 || inner.closeCalls != 1 {
		t.Fatalf("second Close reached the runner: outer = %d, composed = %d",
			recorder.closeCalls, inner.closeCalls)
	}
}

// A decorator that fails or returns nil is a build error, not a runtime with a
// missing boundary: New closes what it built and returns.
func TestSandboxDecoratorFailsBuild(t *testing.T) {
	t.Run("nil runner", func(t *testing.T) {
		_, err := New(t.Context(), localConfig(t), WithSandboxDecorator(
			func(Sandbox, sandbox.Runner) (sandbox.Runner, error) { return nil, nil }))
		if !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "nil runner") {
			t.Fatalf("err = %v, want a validation error naming the nil runner", err)
		}
	})
	t.Run("error", func(t *testing.T) {
		boom := errors.New("boom")
		_, err := New(t.Context(), localConfig(t), WithSandboxDecorator(
			func(Sandbox, sandbox.Runner) (sandbox.Runner, error) { return nil, boom }))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
	})
}

// The journal is opened against the decorated runner, so a decorator that does
// not forward JournalProvider turns a configured journal into a build failure
// (and, worse, a Capabilities declaration that lies).
func TestSandboxDecoratorKeepsTheJournal(t *testing.T) {
	cfg := localConfig(t)
	cfg.Sandbox.Journal = &journal.Settings{Exclude: []string{".git"}}

	var (
		recorder *recordingRunner
		inner    *closeRecorder
	)
	rt, err := New(t.Context(), cfg, recordingDecorator(t, &recorder, &inner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	if rt.Journal() == nil {
		t.Fatal("the configured journal did not survive the decorator")
	}

	// The trap engine.Forwarder exists for: embedding sandbox.Runner promotes
	// neither OpenJournal nor Attach, so New has to fail rather than run with a
	// journal the capabilities still advertise.
	t.Run("a decorator that drops the journal fails the build", func(t *testing.T) {
		cfg := localConfig(t)
		cfg.Sandbox.Journal = &journal.Settings{}

		_, err := New(t.Context(), cfg, WithSandboxDecorator(
			func(_ Sandbox, runner sandbox.Runner) (sandbox.Runner, error) {
				return &lossyRunner{runner}, nil
			}))
		if !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "OpenJournal") {
			t.Fatalf("err = %v, want a validation error naming OpenJournal", err)
		}
	})
}

// lossyRunner embeds the interface, which is the documented trap: it has no
// OpenJournal or Attach even when the runner it wraps does.
type lossyRunner struct{ sandbox.Runner }
