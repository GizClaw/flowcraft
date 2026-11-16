package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// errForwarded is the sentinel every forwarded call returns, so a test can tell
// "the call reached the inner runner" from "a stub answered".
var errForwarded = errors.New("forwarded to inner")

// forwardInner provides every capability a decorator could swallow:
// Runner, JournalProvider and SessionAttacher.
type forwardInner struct {
	closeCalls int
	specs      []sandbox.SessionSpec
	terminated string
}

func (r *forwardInner) Close() error {
	r.closeCalls++
	return nil
}

func (r *forwardInner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Policy: sandbox.Enforcement{CPUCap: true}}
}

func (r *forwardInner) Start(
	_ context.Context, spec sandbox.SessionSpec,
) (sandbox.Session, error) {
	r.specs = append(r.specs, spec)
	return nil, errForwarded
}

func (r *forwardInner) List(context.Context) ([]sandbox.SessionInfo, error) {
	return nil, errForwarded
}

func (r *forwardInner) Terminate(_ context.Context, id string) error {
	r.terminated = id
	return errForwarded
}

func (r *forwardInner) OpenJournal(context.Context) (sandbox.FileJournal, error) {
	return nil, errForwarded
}

func (r *forwardInner) Attach(context.Context, string) (sandbox.Session, error) {
	return nil, errForwarded
}

// plainInner is a runner with no optional capability at all.
type plainInner struct{}

func (*plainInner) Close() error                                        { return nil }
func (*plainInner) Capabilities() sandbox.Capabilities                  { return sandbox.Capabilities{} }
func (*plainInner) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }
func (*plainInner) Terminate(context.Context, string) error             { return nil }

func (*plainInner) Start(context.Context, sandbox.SessionSpec) (sandbox.Session, error) {
	return nil, errForwarded
}

func TestNewForwarderRejectsNil(t *testing.T) {
	if _, err := engine.NewForwarder(nil); !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want a validation error", err)
	}
}

// Every Runner method reaches the inner runner: forwarding must not become an
// interception by accident.
func TestForwarderForwardsRunner(t *testing.T) {
	inner := &forwardInner{}
	fwd, err := engine.NewForwarder(inner)
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}

	if got := fwd.Capabilities(); !got.Policy.CPUCap {
		t.Fatalf("Capabilities = %+v, want the inner declaration", got)
	}
	if _, err := fwd.Start(context.Background(), sandbox.SessionSpec{Argv: []string{"sh"}}); !errors.Is(err, errForwarded) {
		t.Fatalf("Start err = %v, want %v", err, errForwarded)
	}
	if len(inner.specs) != 1 {
		t.Fatalf("inner Start calls = %d, want 1", len(inner.specs))
	}
	if _, err := fwd.List(context.Background()); !errors.Is(err, errForwarded) {
		t.Fatalf("List err = %v, want %v", err, errForwarded)
	}
	if err := fwd.Terminate(context.Background(), "s-1"); !errors.Is(err, errForwarded) {
		t.Fatalf("Terminate err = %v, want %v", err, errForwarded)
	}
	if inner.terminated != "s-1" {
		t.Fatalf("terminated = %q, want s-1", inner.terminated)
	}
	if err := fwd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if inner.closeCalls != 1 {
		t.Fatalf("inner Close calls = %d, want 1", inner.closeCalls)
	}
}

// The optional capabilities are the ones a naive wrapper loses: embedding
// sandbox.Runner promotes neither, so the explicit forwarding is what keeps
// Capabilities honest.
func TestForwarderForwardsOptionalCapabilities(t *testing.T) {
	fwd, err := engine.NewForwarder(&forwardInner{})
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}

	if _, err := sandbox.OpenJournal(context.Background(), fwd); !errors.Is(err, errForwarded) {
		t.Fatalf("OpenJournal err = %v, want %v", err, errForwarded)
	}
	if _, err := sandbox.Attach(context.Background(), fwd, "s-1"); !errors.Is(err, errForwarded) {
		t.Fatalf("Attach err = %v, want %v", err, errForwarded)
	}
}

// An inner runner without a journal must still answer through the forwarder:
// NotAvailable is the contract answer, not a missing method.
func TestForwarderKeepsJournalNotAvailable(t *testing.T) {
	fwd, err := engine.NewForwarder(&plainInner{})
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}
	if _, err := sandbox.OpenJournal(context.Background(), fwd); !errdefs.IsNotAvailable(err) {
		t.Fatalf("OpenJournal err = %v, want NotAvailable", err)
	}
}

// Forwarder must not opt into the one-shot override: a decorator that changes
// neither Exec nor Start keeps the derived view over Start.
func TestForwarderDoesNotOverrideOneShot(t *testing.T) {
	fwd, err := engine.NewForwarder(&forwardInner{})
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}
	if _, ok := any(fwd).(sandbox.Executor); ok {
		t.Fatal("Forwarder implements sandbox.Executor; a plain decorator must keep the derived one-shot path")
	}
	if _, err := engine.Exec(context.Background(), fwd, "true", nil, sandbox.ExecOptions{}); !errors.Is(err, errForwarded) {
		t.Fatalf("Exec err = %v, want the Start-derived path to reach the inner runner", err)
	}
}
