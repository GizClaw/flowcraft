package read

import (
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestAllSourcesFailed_Empty(t *testing.T) {
	if err := AllSourcesFailed(nil); err != nil {
		t.Fatalf("nil errs: %v", err)
	}
}

func TestAllSourcesFailed_AllNotAvailable(t *testing.T) {
	errs := []error{
		errdefs.NotAvailablef("retrieval: down"),
		errdefs.NotAvailablef("entity: down"),
	}
	err := AllSourcesFailed(errs)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("want NotAvailable, got %v", err)
	}
}

func TestAllSourcesFailed_MixedIsInternal(t *testing.T) {
	errs := []error{
		errdefs.NotAvailablef("retrieval: down"),
		errors.New("entity: plain"),
	}
	err := AllSourcesFailed(errs)
	if !errdefs.IsInternal(err) {
		t.Fatalf("want Internal, got %v", err)
	}
}
