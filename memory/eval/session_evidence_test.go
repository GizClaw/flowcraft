package eval

import (
	"context"
	"testing"

	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// sessionEvidenceScenario holds two sessions named by turn-id suffix: s1 has
// two messages, s2 has one.
func sessionEvidenceScenario() Scenario {
	return Scenario{
		Name: "session-evidence", Scope: Scope{RuntimeID: "memories"}, ConversationID: "conv-1",
		Turns: []Turn{
			{
				IdempotencyKey: "t1",
				DatasetIDs:     []string{"s1#0", "s1#1"},
				Messages: []coremessage.Message{
					coremessage.NewTextMessage(coremessage.RoleUser, "Alice adopted a dog."),
					coremessage.NewTextMessage(coremessage.RoleAssistant, "Congrats!"),
				},
			},
			{
				IdempotencyKey: "t2",
				DatasetIDs:     []string{"s2#0"},
				Messages: []coremessage.Message{
					coremessage.NewTextMessage(coremessage.RoleUser, "Bob likes coffee."),
				},
			},
		},
		Questions: []Question{
			{
				Query: "dog", EvidenceSessions: []string{"s1"},
				Category: LongMemEvalMultiSession, CategoryLabel: "multi-session",
			},
			{
				Query: "dog and coffee", EvidenceSessions: []string{"s1", "s2"},
				Category: LongMemEvalMultiSession, CategoryLabel: "multi-session",
			},
			{
				Query: "gold session the haystack does not hold", EvidenceSessions: []string{"s3"},
				Category: LongMemEvalKnowledgeUpdate, CategoryLabel: "knowledge-update",
			},
		},
	}
}

// Session-level evidence counts a gold session once: it is recalled when any
// message of it was surfaced, not when every one was. Grading it per message
// would make a hit a function of how long the gold session happens to be, and
// a three-session question could never hit under a 20-item budget.
func TestRunGradesSessionEvidence(t *testing.T) {
	report, err := Run(context.Background(), stubRunner{answer: "Alice adopted a dog."}, sessionEvidenceScenario())
	if err != nil {
		t.Fatal(err)
	}
	questions := report.Questions
	if got := questions[0]; !got.Hit || got.SessionTotal != 1 || got.SessionFound != 1 {
		t.Fatalf("single gold session = %#v", got)
	}
	if questions[0].CategoryLabel != "multi-session" || questions[0].Category != LongMemEvalMultiSession {
		t.Fatalf("category label did not survive grading: %#v", questions[0])
	}
	// s1 is surfaced by its first message's text; s2 is not.
	if got := questions[1]; got.Hit || got.SessionTotal != 2 || got.SessionFound != 1 {
		t.Fatalf("two gold sessions = %#v", got)
	}
	if missing := questions[1].Missing; len(missing) != 1 || missing[0] != "s2" {
		t.Fatalf("missing sessions = %#v", missing)
	}
	// A gold session with no rendered message stays in the denominator: dropping
	// it would grade a question the conversion cannot support as a free hit.
	if got := questions[2]; got.Hit || got.SessionTotal != 1 || got.SessionFound != 0 {
		t.Fatalf("unrendered gold session = %#v", got)
	}
	if report.Hits != 1 || report.HitRate != 1.0/3.0 {
		t.Fatalf("report = %#v", report)
	}
	if report.SessionTotal != 4 || report.SessionFound != 2 || report.SessionRecall != 0.5 {
		t.Fatalf("session rollup = %#v", report)
	}
	// The turn-level counters stay empty: the two units are never added together.
	if report.EvidenceTotal != 0 || report.EvidenceFound != 0 {
		t.Fatalf("session evidence leaked into the turn counters: %#v", report)
	}
	stats, ok := report.ByCategory[LongMemEvalMultiSession]
	if !ok || stats.Label != "multi-session" || stats.Questions != 2 || stats.Hits != 1 {
		t.Fatalf("category rollup = %#v", report.ByCategory)
	}
}

// A dataset that carries both labels keeps its turn-level evidence: session
// grading is an alternative, not an extra pass over the same gold.
func TestTurnEvidenceStillWinsWhenTheDatasetNamesTurns(t *testing.T) {
	scenario := sessionEvidenceScenario()
	scenario.Questions = []Question{{
		Query: "dog", Evidence: []string{"s1#0"}, EvidenceSessions: []string{"s2"},
	}}
	report, err := Run(context.Background(), stubRunner{answer: "Alice adopted a dog."}, scenario)
	if err != nil {
		t.Fatal(err)
	}
	got := report.Questions[0]
	if got.EvidenceTotal != 1 || got.EvidenceFound != 1 || got.SessionTotal != 0 || !got.Hit {
		t.Fatalf("turn evidence = %#v", got)
	}
}
