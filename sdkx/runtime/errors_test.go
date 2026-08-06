package runtime

import (
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestErrBuilderUsedIsConflict(t *testing.T) {
	if err := fmt.Errorf("wrapped: %w", ErrBuilderUsed); !errdefs.IsConflict(err) {
		t.Fatalf("ErrBuilderUsed classification = %v, want conflict", err)
	}
}
