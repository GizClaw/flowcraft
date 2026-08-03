package memorytest

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/memory"
)

// ArchiveSuite drives the documented mutating maintenance contract:
// the request is validated and the implementation reports the
// completed archive move.
type ArchiveSuite struct {
	Spec         memory.Spec
	BuildRuntime func(t *testing.T) *memory.Runtime

	SampleScope memory.Scope
}

func RunArchive(t *testing.T, s ArchiveSuite) {
	t.Helper()
	if s.BuildRuntime == nil {
		t.Fatal("ArchiveSuite.BuildRuntime is required")
	}
	if s.SampleScope.RuntimeID == "" {
		t.Fatal("ArchiveSuite.SampleScope.RuntimeID is required")
	}

	ctx := withCtx(t, defaultTestTimeout)

	t.Run("returns_non_negative_response", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		resp, err := rt.ExecuteArchive(ctx, memory.ArchiveRequest{
			Scope:       s.SampleScope,
			OlderThan:   time.Now().Add(-4320 * time.Hour),
			Destination: "memory://archive",
		})
		if err != nil {
			t.Fatalf("ExecuteArchive: %v", err)
		}
		if resp.Archived < 0 {
			t.Errorf("Archived = %d, want >= 0", resp.Archived)
		}
		if resp.Bytes < 0 {
			t.Errorf("Bytes = %d, want >= 0", resp.Bytes)
		}
	})

	t.Run("scope_mismatch_returns_kind_scope_invalid", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		_, err := rt.ExecuteArchive(ctx, memory.ArchiveRequest{
			Scope:     memory.Scope{RuntimeID: "wrong"},
			OlderThan: time.Now().Add(-4320 * time.Hour),
		})
		memErr := memory.AsError(err)
		if memErr == nil || memErr.Kind != memory.KindScopeInvalid {
			t.Errorf("expected KindScopeInvalid, got: %v", err)
		}
	})
}
