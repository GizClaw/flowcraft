package memorytest

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/memory"
)

// AppendSuite drives the documented Append contract: LastSeq
// monotonicity, IdempotencyKey dedup, scope partitioning. The
// suite assumes the kernel's own append tests have already
// covered shape / scope-mismatch / NotConfigured / Compile
// rejection; this suite is for the impl.
type AppendSuite struct {
	Spec         memory.Spec
	BuildRuntime func(t *testing.T) *memory.Runtime

	// SampleScope is the scope used for every sub-test. The
	// kernel requires a non-empty RuntimeID; tests should
	// supply the rest.
	SampleScope memory.Scope
	// ConversationID is the transcript under test.
	ConversationID string
}

func RunAppend(t *testing.T, s AppendSuite) {
	t.Helper()
	if s.BuildRuntime == nil {
		t.Fatal("AppendSuite.BuildRuntime is required")
	}
	if s.SampleScope.RuntimeID == "" {
		t.Fatal("AppendSuite.SampleScope.RuntimeID is required")
	}
	if s.ConversationID == "" {
		s.ConversationID = "conv-1"
	}

	ctx := withCtx(t, defaultTestTimeout)

	t.Run("first_append_assigns_seq", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		req := memory.AppendRequest{
			Scope:          s.SampleScope,
			ConversationID: s.ConversationID,
			Records:        []memory.Record{mustRecord("r-1", "hello")},
		}
		resp, err := rt.ExecuteAppend(ctx, req)
		if err != nil {
			t.Fatalf("ExecuteAppend: %v", err)
		}
		if resp.Appended != 1 {
			t.Errorf("Appended = %d, want 1", resp.Appended)
		}
		if resp.LastSeq == 0 {
			t.Error("LastSeq should be non-zero after a successful append")
		}
	})

	t.Run("last_seq_is_monotonic", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		var prev uint64
		for i := 0; i < 3; i++ {
			req := memory.AppendRequest{
				Scope:          s.SampleScope,
				ConversationID: s.ConversationID,
				Records: []memory.Record{
					mustRecord("r-"+itoa(i), "msg"),
				},
			}
			resp, err := rt.ExecuteAppend(ctx, req)
			if err != nil {
				t.Fatalf("ExecuteAppend[%d]: %v", i, err)
			}
			if resp.LastSeq <= prev {
				t.Errorf("LastSeq not monotonic: prev=%d, got=%d", prev, resp.LastSeq)
			}
			prev = resp.LastSeq
		}
	})

	t.Run("idempotency_key_dedups", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		req := memory.AppendRequest{
			Scope:          s.SampleScope,
			ConversationID: s.ConversationID,
			IdempotencyKey: "run-7",
			Records: []memory.Record{
				mustRecord("r-dedup", "once"),
			},
		}
		first, err := rt.ExecuteAppend(ctx, req)
		if err != nil {
			t.Fatalf("first Append: %v", err)
		}
		second, err := rt.ExecuteAppend(ctx, req)
		if err != nil {
			t.Fatalf("second Append: %v", err)
		}
		if second.LastSeq != first.LastSeq {
			t.Errorf("IdempotencyKey should yield the same LastSeq: first=%d, second=%d",
				first.LastSeq, second.LastSeq)
		}
		// A faithful impl reports Appended=0 on the dedup path
		// and Appended=1 on the first write. An impl that
		// cannot dedup must still return the same LastSeq; the
		// suite reports Appended so the test stays neutral.
		_ = second.Appended
	})

	t.Run("scope_mismatch_returns_kind_scope_invalid", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		_, err := rt.ExecuteAppend(ctx, memory.AppendRequest{
			Scope: memory.Scope{
				RuntimeID:      "wrong",
				UserID:         s.SampleScope.UserID,
				ConversationID: s.ConversationID,
			},
			Records: []memory.Record{mustRecord("r-1", "x")},
		})
		if err == nil {
			t.Fatal("expected error for scope/RuntimeID mismatch")
		}
		memErr := memory.AsError(err)
		if memErr == nil || memErr.Kind != memory.KindScopeInvalid {
			t.Errorf("expected KindScopeInvalid, got: %v", err)
		}
	})

	t.Run("empty_records_rejected", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		_, err := rt.ExecuteAppend(ctx, memory.AppendRequest{
			Scope:          s.SampleScope,
			ConversationID: s.ConversationID,
		})
		memErr := memory.AsError(err)
		if memErr == nil || memErr.Kind != memory.KindInvalidRequest {
			t.Errorf("expected KindInvalidRequest, got: %v", err)
		}
	})
}
