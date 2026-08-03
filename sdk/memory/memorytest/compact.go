package memorytest

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/memory"
)

// CompactSuite drives the documented mutating maintenance contract:
// the request is validated and the implementation reports the
// completed compaction.
type CompactSuite struct {
	Spec         memory.Spec
	BuildRuntime func(t *testing.T) *memory.Runtime

	SampleScope    memory.Scope
	ConversationID string
}

func RunCompact(t *testing.T, s CompactSuite) {
	t.Helper()
	if s.BuildRuntime == nil {
		t.Fatal("CompactSuite.BuildRuntime is required")
	}
	if s.SampleScope.RuntimeID == "" {
		t.Fatal("CompactSuite.SampleScope.RuntimeID is required")
	}
	if s.ConversationID == "" {
		s.ConversationID = "conv-1"
	}

	ctx := withCtx(t, defaultTestTimeout)

	t.Run("returns_non_negative_response", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		resp, err := rt.ExecuteCompact(ctx, memory.CompactRequest{
			Scope:     s.SampleScope,
			OlderThan: time.Now().Add(-720 * time.Hour),
			Keep:      50,
		})
		if err != nil {
			t.Fatalf("ExecuteCompact: %v", err)
		}
		if resp.Compacted < 0 {
			t.Errorf("Compacted = %d, want >= 0", resp.Compacted)
		}
		if resp.Bytes < 0 {
			t.Errorf("Bytes = %d, want >= 0", resp.Bytes)
		}
	})

	t.Run("scope_mismatch_returns_kind_scope_invalid", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		_, err := rt.ExecuteCompact(ctx, memory.CompactRequest{
			Scope:     memory.Scope{RuntimeID: "wrong"},
			OlderThan: time.Now().Add(-720 * time.Hour),
		})
		memErr := memory.AsError(err)
		if memErr == nil || memErr.Kind != memory.KindScopeInvalid {
			t.Errorf("expected KindScopeInvalid, got: %v", err)
		}
	})
}
