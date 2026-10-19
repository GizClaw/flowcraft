package sandbox_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/local"
)

// closingRunner records whether Close reached it, so tests can prove
// that decoration does not hide the inner runner's lifecycle.
type closingRunner struct {
	closed bool
}

func (r *closingRunner) Close() error {
	r.closed = true
	return nil
}

func (r *closingRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{}
}

func (r *closingRunner) Start(context.Context, sandbox.SessionSpec) (sandbox.Session, error) {
	return nil, nil
}

func (r *closingRunner) List(context.Context) ([]sandbox.SessionInfo, error) {
	return nil, nil
}

func (r *closingRunner) Terminate(context.Context, string) error {
	return nil
}

// TestDecoratorsForwardClose exercises the full recommended chain —
// WithDefaults(WithApproval(AllowCommands(inner))) — and asserts that
// Close reaches the backend exactly once through every decorator.
func TestDecoratorsForwardClose(t *testing.T) {
	inner := &closingRunner{}
	runner := sandbox.WithDefaults(
		sandbox.WithApproval(
			sandbox.AllowCommands(inner, nil),
			nil, nil,
		),
		sandbox.ExecOptions{},
	)

	closer, ok := runner.(io.Closer)
	if !ok {
		t.Fatal("decorated runner must implement io.Closer")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inner.closed {
		t.Fatal("Close was not forwarded to the inner runner")
	}
}

// TestDefaultsStdinDoesNotBuyAPipe covers the merge path that never
// touches Runner.Exec: WithDefaults copies one-shot bytes onto a spec the
// caller built with Stdin false, and those bytes must not become an input
// side. Start never delivers Opts.Stdin, so a pipe built for them would
// have no writer and no close — the child would hang on it.
func TestDefaultsStdinDoesNotBuyAPipe(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runner := sandbox.WithDefaults(local.New(t.TempDir()),
		sandbox.ExecOptions{Stdin: []byte("x")})
	sess, err := runner.Start(ctx, sandbox.SessionSpec{
		Argv: []string{"sh", "-c", fd0Check},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if sess.Capabilities().Stdin {
		t.Error("Capabilities().Stdin = true, want false: Start does not deliver Opts.Stdin")
	}
	if out := strings.TrimSpace(drainSession(t, ctx, sess)); out != "chardev" {
		t.Fatalf("fd 0 = %q, want chardev (injected bytes are Exec's input, not a session's)", out)
	}
	if err := sess.Write(ctx, []byte("x")); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Write error = %v, want NotAvailable", err)
	}
}

// TestDefaultsStdinDoesNotStallTheChild is the hang half of the same
// case, in the shape a tool meets it: a child that reads until EOF has to
// exit on its own, because the merged bytes never declared an input side.
func TestDefaultsStdinDoesNotStallTheChild(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runner := sandbox.WithDefaults(local.New(t.TempDir()),
		sandbox.ExecOptions{Stdin: []byte("x\n")})
	sess, err := runner.Start(ctx, sandbox.SessionSpec{Argv: []string{"cat"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if _, err := sess.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v (the child is blocked on a pipe nobody writes or closes?)", err)
	}
}

// TestRunnerCloseIsIdempotent verifies the contract that Close is safe
// to call more than once and when nothing was ever started.
func TestRunnerCloseIsIdempotent(t *testing.T) {
	runner := sandbox.WithDefaults(
		sandbox.WithApproval(
			sandbox.AllowCommands(&closingRunner{}, nil),
			nil, nil,
		),
		sandbox.ExecOptions{},
	)
	for i := 0; i < 2; i++ {
		if err := runner.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
}
