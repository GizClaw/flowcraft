package maintain

import (
	"testing"
	"time"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func planFact(id, text string, entities []string, eventTime time.Time) factview.Fact {
	return factview.Fact{
		ID: id, Scope: corememory.Scope{RuntimeID: "runtime"}, ConversationID: "conv-1",
		Text: text, Entities: entities, EventTime: eventTime, CreatedAt: eventTime,
	}
}

// TestDetectSupersedesContradictingStableFact pins the contradiction fixture:
// when a newer fact restates the same entity's attribute, the older fact is
// marked superseded by the newer one.
func TestDetectSupersedesContradictingStableFact(t *testing.T) {
	older := planFact("old", "Caroline works at Acme Corporation since 2023.",
		[]string{"Caroline", "Acme Corporation"}, time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := planFact("new", "Caroline works at Beta Corporation since 2024.",
		[]string{"Caroline", "Beta Corporation"}, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	plan, err := Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {newer, older},
	}, newer.EventTime.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 1 || plan.Supersedes[0].FactID != "old" ||
		plan.Supersedes[0].SupersededBy != "new" || plan.Supersedes[0].Similarity < 0.5 {
		t.Fatalf("plan = %#v", plan)
	}
	// A different entity must not be superseded.
	unrelated := planFact("other", "Melanie works at Acme Corporation since 2023.",
		[]string{"Melanie", "Acme Corporation"}, time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	plan, err = Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {unrelated, newer},
	}, newer.EventTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 0 {
		t.Fatalf("unrelated facts superseded: %#v", plan.Supersedes)
	}
}

// TestDetectDecaysAgedFacts pins the decay half-life.
func TestDetectDecaysAgedFacts(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	aged := planFact("aged", "Caroline works at Acme Corporation.", []string{"Caroline"}, now.Add(-800*time.Hour))
	fresh := planFact("fresh", "Caroline lives in Shanghai.", []string{"Caroline"}, now.Add(-24*time.Hour))
	plan, err := Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {aged, fresh},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Decays) != 1 || plan.Decays[0].FactID != "aged" || plan.Decays[0].Score >= 0.5 {
		t.Fatalf("plan = %#v", plan)
	}
	for _, decay := range plan.Decays {
		if decay.FactID == "fresh" {
			t.Fatalf("fresh fact decayed: %#v", plan.Decays)
		}
	}
}

func TestDetectValidatesConfigAndScope(t *testing.T) {
	if _, err := Detect(corememory.Scope{}, nil, time.Now()); err == nil {
		t.Fatal("invalid scope accepted")
	}
	_, err := DetectWithConfig(corememory.Scope{RuntimeID: "runtime"}, nil, time.Now(),
		Config{SimilarityThreshold: 2})
	if err == nil {
		t.Fatal("invalid similarity threshold accepted")
	}
}
