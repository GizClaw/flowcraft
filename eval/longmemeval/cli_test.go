package longmemeval

import "testing"

func TestLMEFlattenSessionsSortsByHaystackDates(t *testing.T) {
	inst := lmeRawInstance{
		HaystackSessionIDs: []string{"s-late", "s-early", "s-mid"},
		HaystackDates: []string{
			"2023/04/10 (Mon) 17:50",
			"2023/04/10 (Mon) 14:47",
			"2023/04/10 (Mon) 17:15",
		},
		HaystackSessions: [][]lmeRawTurn{
			{{Role: "user", Content: "late", HasAnswer: true}},
			{{Role: "assistant", Content: "early"}},
			{{Role: "user", Content: "mid"}},
		},
	}

	turns, evidenceIDs := lmeFlattenSessions(inst, "q1")
	if len(turns) != 3 {
		t.Fatalf("turns = %d, want 3", len(turns))
	}
	if got, want := turns[0].SessionID, "q1:s-early"; got != want {
		t.Fatalf("first session = %q, want %q", got, want)
	}
	if got, want := turns[1].SessionID, "q1:s-mid"; got != want {
		t.Fatalf("second session = %q, want %q", got, want)
	}
	if got, want := turns[2].SessionID, "q1:s-late"; got != want {
		t.Fatalf("third session = %q, want %q", got, want)
	}
	if got, want := evidenceIDs[0], "q1:s-late:t0"; got != want {
		t.Fatalf("evidence id = %q, want %q", got, want)
	}
}

func TestLMESessionOrderPreservesInputWhenDateMissing(t *testing.T) {
	inst := lmeRawInstance{
		HaystackDates:    []string{"2023/04/10 (Mon) 17:50"},
		HaystackSessions: [][]lmeRawTurn{{}, {}},
	}
	order := lmeSessionOrder(inst)
	if len(order) != 2 || order[0] != 0 || order[1] != 1 {
		t.Fatalf("order = %+v, want [0 1]", order)
	}
}

func TestLMEFallbackEvidenceIDsExpandsAnswerSessionsToTurns(t *testing.T) {
	inst := lmeRawInstance{
		AnswerSessionIDs:   []string{"s2"},
		HaystackSessionIDs: []string{"s1", "s2"},
		HaystackDates:      []string{"2023/04/10 (Mon) 17:50", "2023/04/10 (Mon) 18:15"},
		HaystackSessions: [][]lmeRawTurn{
			{{Role: "user", Content: "not evidence"}},
			{
				{Role: "user", Content: "supporting turn"},
				{Role: "assistant", Content: "  "},
				{Role: "assistant", Content: "another supporting turn"},
			},
		},
	}

	got := lmeFallbackEvidenceIDs(inst, "q1")
	want := []string{"q1:s2:t0", "q1:s2:t2"}
	if len(got) != len(want) {
		t.Fatalf("fallback evidence ids = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fallback evidence ids = %+v, want %+v", got, want)
		}
	}
}
