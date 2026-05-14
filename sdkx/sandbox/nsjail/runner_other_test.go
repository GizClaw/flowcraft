//go:build !linux

package nsjail

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
