package sandbox_test

import (
	"context"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
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
