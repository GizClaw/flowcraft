package eval

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNewLibraryStateSummarizesCursors pins the block that says which generation
// of derived facts a run graded. The fingerprint's policy digest names the code,
// and a -skip-derive run answers from whatever the store already holds, so
// without this the same workspace read by two derivation policies produced
// identical fingerprints.
func TestNewLibraryStateSummarizesCursors(t *testing.T) {
	state := NewLibraryState(
		"policy-a",
		[]ScopeState{{
			RuntimeID: "memories",
			Watermarks: []ConversationState{
				{ConversationID: "conv-b", Watermark: 7, Behind: true},
				{ConversationID: "conv-a", Watermark: 0},
				{ConversationID: "conv-c", Watermark: 12},
			},
		}},
	)
	if state.Conversations != 3 || state.Underived != 1 || state.Behind != 1 {
		t.Fatalf("counts = conversations=%d underived=%d behind=%d, want 3/1/1",
			state.Conversations, state.Underived, state.Behind)
	}
	if state.Scopes[0].MaxWatermark != 12 {
		t.Fatalf("max watermark = %d, want 12", state.Scopes[0].MaxWatermark)
	}
	// Watermarks are sorted, so the report and the digest do not depend on the
	// store's iteration order.
	ids := make([]string, 0, 3)
	for _, conversation := range state.Scopes[0].Watermarks {
		ids = append(ids, conversation.ConversationID)
	}
	if got := strings.Join(ids, ","); got != "conv-a,conv-b,conv-c" {
		t.Fatalf("watermarks = %s, want conv-a,conv-b,conv-c", got)
	}

	// The digest identifies the generation: a different cursor is a different
	// generation, and the order the scopes arrive in is not part of it.
	reordered := NewLibraryState(
		"policy-a",
		[]ScopeState{{
			RuntimeID: "memories",
			Watermarks: []ConversationState{
				{ConversationID: "conv-c", Watermark: 12},
				{ConversationID: "conv-a"},
				{ConversationID: "conv-b", Watermark: 7, Behind: true},
			},
		}},
	)
	if reordered.Digest != state.Digest {
		t.Fatalf("digest depends on cursor order: %s vs %s", reordered.Digest, state.Digest)
	}
	advanced := NewLibraryState(
		"policy-a",
		[]ScopeState{{
			RuntimeID: "memories",
			Watermarks: []ConversationState{
				{ConversationID: "conv-a", Watermark: 1},
				{ConversationID: "conv-b", Watermark: 7},
				{ConversationID: "conv-c", Watermark: 12},
			},
		}},
	)
	if advanced.Digest == state.Digest {
		t.Fatal("advancing a watermark did not change the library digest")
	}
	if got := advanced.String(); !strings.Contains(got, "underived=0") {
		t.Fatalf("library summary = %q", got)
	}
	if !(LibraryState{}).Empty() || state.Empty() {
		t.Fatal("Empty must distinguish an unread store from a summarized one")
	}
}

// TestBaselineCarriesTheRunMaterialAndCost pins the envelope change: a stored
// report must say how the dataset was converted (including why images are
// missing), which models spent how many tokens, and which derivation it graded.
// These describe the run, not the configuration, so they live outside the
// fingerprint -- putting them in it would make -resume refuse a continuation it
// should accept.
func TestBaselineCarriesTheRunMaterialAndCost(t *testing.T) {
	report := Report{Scenario: "locomo/conv-26", HitRate: 0.5}
	baseline := NewBaseline("report.json", []Report{report})
	baseline.Loader = &LoaderStats{
		Conversations: 10, Turns: 272, Questions: 1540,
		ImagesAttached: 689, ImagesFailed: 221, ImagesShrunk: 6,
		ImageFailures: ImageFailureCounts{Permanent: 160, Busy: 20, Transient: 35, Oversized: 6},
	}
	baseline.Usage = []ModelUsage{{
		Role: "answer", Model: "deepseek/deepseek-flash",
		Calls: 1540, InputTokens: 4_000_000, OutputTokens: 2_700_000,
	}}
	library := NewLibraryState("policy-a", []ScopeState{{
		RuntimeID: "memories",
		Watermarks: []ConversationState{
			{ConversationID: "conv-26", Watermark: 9},
			// A conversation the store knows but this policy never derived: its
			// facts, if any, belong to another generation.
			{ConversationID: "conv-30"},
		},
	}})
	baseline.Library = &library

	encoded, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"images_shrunk":6`, `"oversized":6`, `"role":"answer"`,
		`"output_tokens":2700000`, `"underived":1`, `"watermark":9`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("stored report does not carry %s:\n%s", want, encoded)
		}
	}

	// A recall-only run must not pretend to have cost anything.
	plain, err := json.Marshal(NewBaseline("report.json", []Report{report}))
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"usage", "library", "loader"} {
		if strings.Contains(string(plain), unwanted) {
			t.Fatalf("an empty envelope carries %q:\n%s", unwanted, plain)
		}
	}

	// And a later reader still accepts a report written before these fields
	// existed: LoadBaseline is strict the other way round (unknown fields are
	// rejected, missing ones are not).
	legacy := []byte(`{"name":"old.json","created_at":"2026-09-24T06:07:31Z","reports":[]}`)
	if _, err := LoadBaseline(strings.NewReader(string(legacy))); err != nil {
		t.Fatalf("a pre-existing report no longer loads: %v", err)
	}
}
