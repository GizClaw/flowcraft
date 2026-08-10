//go:build !linux

package bwrap

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestNew_NotAvailableOnNonLinux(t *testing.T) {
	r, err := New(t.TempDir())
	if r != nil {
		t.Errorf("expected nil Runner on non-Linux")
	}
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable, got %v", err)
	}
}

// TestStubRunner_ExecNotAvailable exercises the unreachable Exec path
// for completeness — the package contract is that a zero Runner
// returns NotAvailable rather than panicking, so portable test code
// that does its own type-assert dance does not crash.
func TestStubRunner_ExecNotAvailable(t *testing.T) {
	var r Runner
	_, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable, got %v", err)
	}
}

// TestStubRunner_ProcessManagerNotAvailable pins the same contract for
// the optional session capability: portable code that discovers it via
// ProcessManagerOf gets NotAvailable, never a silent downgrade.
func TestStubRunner_ProcessManagerNotAvailable(t *testing.T) {
	var r Runner
	pm := sandbox.ProcessManagerOf(&r)
	if pm == nil {
		t.Fatal("stub Runner must implement ProcessManager for type assertions")
	}
	if _, err := pm.Start(context.Background(), sandbox.ProcessSpec{Argv: []string{"/bin/true"}}); !errdefs.IsNotAvailable(err) {
		t.Errorf("Start = %v, want NotAvailable", err)
	}
	if _, err := pm.List(context.Background()); !errdefs.IsNotAvailable(err) {
		t.Errorf("List = %v, want NotAvailable", err)
	}
	if err := pm.Terminate(context.Background(), "x"); !errdefs.IsNotAvailable(err) {
		t.Errorf("Terminate = %v, want NotAvailable", err)
	}
}
