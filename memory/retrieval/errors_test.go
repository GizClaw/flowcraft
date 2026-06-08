package retrieval

import (
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestErrNoQueryIsValidation(t *testing.T) {
	if !errdefs.IsValidation(ErrNoQuery) {
		t.Fatalf("ErrNoQuery should classify as validation")
	}
	if !errors.Is(ErrNoQuery, ErrNoQuery) {
		t.Fatalf("errors.Is should match the sentinel itself")
	}
}
