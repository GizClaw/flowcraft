package diagnostics_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestDiagnoseAsyncSemanticQueue_Backlog(t *testing.T) {
	h := diagnostics.DiagnoseAsyncSemanticQueue(port.AsyncSemanticStats{
		Pending:        3,
		Leased:         2,
		DeadLetter:     1,
		CancelledTotal: 4,
	})
	if h.Backlog != 5 {
		t.Fatalf("Backlog = %d, want 5", h.Backlog)
	}
	if h.DeadLetter != 1 || h.CancelledTotal != 4 {
		t.Fatalf("health = %+v", h)
	}
}
