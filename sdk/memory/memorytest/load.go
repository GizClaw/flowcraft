package memorytest

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/memory"
)

// LoadSuite drives the documented Load contract: Limit caps the
// result, Cursor / NextCursor paginate, and the kernel rejects
// unbounded Load (Limit == 0 with no default). The suite
// exercises a real impl: callers pre-populate the transcript
// via [AppendSuite]-style fixtures (or directly via the runtime)
// and the suite then drives Load through the documented shapes.
type LoadSuite struct {
	Spec         memory.Spec
	BuildRuntime func(t *testing.T) *memory.Runtime

	SampleScope    memory.Scope
	ConversationID string
}

func RunLoad(t *testing.T, s LoadSuite) {
	t.Helper()
	if s.BuildRuntime == nil {
		t.Fatal("LoadSuite.BuildRuntime is required")
	}
	if s.SampleScope.RuntimeID == "" {
		t.Fatal("LoadSuite.SampleScope.RuntimeID is required")
	}
	if s.ConversationID == "" {
		s.ConversationID = "conv-1"
	}

	ctx := withCtx(t, defaultTestTimeout)

	// seedTranscript is a small helper that appends N records
	// to the same conversation. Tests use it to set up the
	// transcript before driving Load.
	seedTranscript := func(t *testing.T, rt *memory.Runtime, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			_, err := rt.ExecuteAppend(ctx, memory.AppendRequest{
				Scope:          s.SampleScope,
				ConversationID: s.ConversationID,
				Records:        []memory.Record{mustRecord("r-"+itoa(i), "msg")},
			})
			if err != nil {
				t.Fatalf("seed append: %v", err)
			}
		}
	}

	t.Run("returns_records_in_order", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		seedTranscript(t, rt, 3)

		resp, err := rt.ExecuteLoad(ctx, memory.LoadRequest{
			Scope:          s.SampleScope,
			ConversationID: s.ConversationID,
			Limit:          10,
		})
		if err != nil {
			t.Fatalf("ExecuteLoad: %v", err)
		}
		if len(resp.Records) != 3 {
			t.Errorf("len(Records) = %d, want 3", len(resp.Records))
		}
		for i, r := range resp.Records {
			if r.ID != "r-"+itoa(i) {
				t.Errorf("Records[%d].ID = %q, want %q", i, r.ID, "r-"+itoa(i))
			}
			if r.Seq == 0 {
				t.Errorf("Records[%d].Seq is zero, impl must assign a Seq", i)
			}
		}
	})

	t.Run("limit_caps_result", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		seedTranscript(t, rt, 5)

		resp, err := rt.ExecuteLoad(ctx, memory.LoadRequest{
			Scope:          s.SampleScope,
			ConversationID: s.ConversationID,
			Limit:          2,
		})
		if err != nil {
			t.Fatalf("ExecuteLoad: %v", err)
		}
		if len(resp.Records) != 2 {
			t.Errorf("len(Records) = %d, want 2", len(resp.Records))
		}
	})

	t.Run("cursor_paginates_full_transcript", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		seedTranscript(t, rt, 5)

		seen := map[string]bool{}
		cursor := ""
		for page := 0; page < 10; page++ {
			resp, err := rt.ExecuteLoad(ctx, memory.LoadRequest{
				Scope:          s.SampleScope,
				ConversationID: s.ConversationID,
				Limit:          2,
				Cursor:         cursor,
			})
			if err != nil {
				t.Fatalf("page %d: %v", page, err)
			}
			if len(resp.Records) == 0 {
				break
			}
			for _, r := range resp.Records {
				if seen[r.ID] {
					t.Errorf("record %q returned twice across pages", r.ID)
				}
				seen[r.ID] = true
			}
			if resp.NextCursor == "" {
				break
			}
			cursor = resp.NextCursor
		}
		if len(seen) != 5 {
			t.Errorf("paged %d distinct records, want 5", len(seen))
		}
	})

	t.Run("reverse_returns_newest_first", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		seedTranscript(t, rt, 3)

		resp, err := rt.ExecuteLoad(ctx, memory.LoadRequest{
			Scope:          s.SampleScope,
			ConversationID: s.ConversationID,
			Limit:          10,
			Reverse:        true,
		})
		if err != nil {
			t.Fatalf("ExecuteLoad: %v", err)
		}
		if len(resp.Records) != 3 {
			t.Fatalf("len(Records) = %d, want 3", len(resp.Records))
		}
		// Newest record is the one with the highest Seq.
		var prev uint64 = ^uint64(0)
		for i, r := range resp.Records {
			if r.Seq > prev {
				t.Errorf("Records[%d].Seq = %d, but should be descending (prev %d)",
					i, r.Seq, prev)
			}
			prev = r.Seq
		}
	})

	t.Run("zero_limit_rejected", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		_, err := rt.ExecuteLoad(ctx, memory.LoadRequest{
			Scope:          s.SampleScope,
			ConversationID: s.ConversationID,
		})
		memErr := memory.AsError(err)
		if memErr == nil || memErr.Kind != memory.KindInvalidRequest {
			t.Errorf("expected KindInvalidRequest, got: %v", err)
		}
	})
}
