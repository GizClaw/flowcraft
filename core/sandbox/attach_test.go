package sandbox_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// attacherRunner is a runner that implements the optional re-attach seam. It
// records what reached Attach so the helper's forwarding is observable without
// a full Session stub.
type attacherRunner struct {
	captureRunner
	gotID string
	err   error
}

func (r *attacherRunner) Attach(_ context.Context, id string) (sandbox.Session, error) {
	r.gotID = id
	return nil, r.err
}

// Attach is the only supported entry point: a caller asks the runner instead of
// asserting the optional interface, so a backend can keep the capability behind
// a decorator chain or behind the wire.
func TestAttach_ForwardsToTheRunner(t *testing.T) {
	sentinel := sandbox.ErrSessionClosed
	r := &attacherRunner{err: sentinel}

	_, err := sandbox.Attach(context.Background(), r, "s1")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Attach error = %v, want the attacher's own error", err)
	}
	if r.gotID != "s1" {
		t.Fatalf("attacher saw id %q, want s1", r.gotID)
	}
}

// A runner that cannot re-attach says so with NotAvailable rather than a lookup
// failure: the capability is absent, not the session.
func TestAttach_UnsupportedRunnerIsNotAvailable(t *testing.T) {
	_, err := sandbox.Attach(context.Background(), &captureRunner{}, "s1")
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("Attach error = %v, want NotAvailable", err)
	}
}

func TestAttach_RejectsMissingInput(t *testing.T) {
	if _, err := sandbox.Attach(context.Background(), nil, "s1"); !errdefs.IsValidation(err) {
		t.Errorf("nil runner: err = %v, want Validation", err)
	}
	r := &attacherRunner{}
	if _, err := sandbox.Attach(context.Background(), r, ""); !errdefs.IsValidation(err) {
		t.Errorf("empty id: err = %v, want Validation", err)
	}
	if r.gotID != "" {
		t.Errorf("attacher was called with %q for a rejected request", r.gotID)
	}
}
