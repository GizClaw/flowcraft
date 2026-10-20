package profile

import (
	"strings"
	"testing"
	"time"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

var testNow = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

func testScope() corememory.Scope {
	return corememory.Scope{RuntimeID: "runtime", UserID: "user"}
}

func testFact(conversationID, id, text string, entities []string, at time.Time) factview.Fact {
	return factview.Fact{
		ID: id, ConversationID: conversationID, Text: text, Entities: entities,
		EventTime: at, CreatedAt: at,
	}
}

// TestDistillKeepsStandingStatementsAndDropsTurns is the rule's contract: what
// a profile line is, and what it is not.
func TestDistillKeepsStandingStatementsAndDropsTurns(t *testing.T) {
	at := testNow.Add(-24 * time.Hour)
	facts := []factview.Fact{
		testFact("conv-1", "fact-reported", "Caroline said she loves creating art and that painting is great for expressing herself.", []string{"caroline"}, at),
		testFact("conv-1", "fact-direct", "Melanie loves pottery and finds it relaxing.", []string{"melanie"}, at),
		testFact("conv-1", "fact-copula", "Caroline is an artist.", []string{"caroline"}, at),
		testFact("conv-1", "fact-possessive", "Melanie's goal is to open her own studio.", []string{"melanie"}, at),
		testFact("conv-1", "fact-habit", "Caroline plays acoustic guitar, having started about five years ago.", []string{"caroline"}, at),
		testFact("conv-1", "fact-event", "On 8 May 2023, Caroline attended a counseling workshop that she found enlightening.", []string{"caroline"}, at),
		testFact("conv-1", "fact-relation", "Caroline praised Melanie's painting and said she loves how it tells a story.", []string{"caroline", "melanie"}, at),
		testFact("conv-1", "fact-noun", "Tim expressed that love is truly magical and brings so much joy.", []string{"tim"}, at),
	}
	entries := Distill(facts, nil, Config{})
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Text)
	}
	if len(got) == 0 {
		t.Fatal("Distill kept nothing")
	}
	kept := strings.Join(got, "\n")
	for _, wanted := range []string{"loves creating art", "Melanie loves pottery", "Caroline is an artist", "goal is to open her own studio", "plays acoustic guitar"} {
		if !strings.Contains(kept, wanted) {
			t.Errorf("block does not hold %q:\n%s", wanted, kept)
		}
	}
	for _, unwanted := range []string{"attended a counseling workshop", "praised Melanie", "love is truly magical"} {
		if strings.Contains(kept, unwanted) {
			t.Errorf("block holds %q, which is not a profile line:\n%s", unwanted, kept)
		}
	}
}

// TestDistillRanksByClassThenRecency pins the order a bound-bound block spends
// itself in: the earlier class first, and inside a class the newer statement.
func TestDistillRanksByClassThenRecency(t *testing.T) {
	older := testNow.Add(-72 * time.Hour)
	newer := testNow.Add(-1 * time.Hour)
	facts := []factview.Fact{
		testFact("conv-1", "fact-goal", "Melanie wants to open her own studio.", []string{"melanie"}, newer),
		testFact("conv-1", "fact-old-preference", "Melanie loves pottery.", []string{"melanie"}, older),
		testFact("conv-1", "fact-new-preference", "Melanie loves live music.", []string{"melanie"}, newer),
	}
	entries := Distill(facts, nil, Config{})
	want := []string{
		"Melanie loves live music.",
		"Melanie loves pottery.",
		"Melanie wants to open her own studio.",
	}
	if len(entries) != len(want) {
		t.Fatalf("Distill returned %d entries, want %d", len(entries), len(want))
	}
	for index, text := range want {
		if entries[index].Text != text {
			t.Errorf("entry %d = %q, want %q", index, entries[index].Text, text)
		}
	}
	if entries[0].ID != EntryID(entries[0].Text) || entries[0].Origin != OriginDistilled {
		t.Errorf("entry is not stamped as a distilled line: %+v", entries[0])
	}
	if len(entries[0].Provenance) != 1 || entries[0].Provenance[0] != "fact-new-preference" {
		t.Errorf("entry provenance = %v, want [fact-new-preference]", entries[0].Provenance)
	}
}

// TestDistillDropsRestatements pins the dedupe: a paraphrase that adds a
// trailing clause is one claim, and it spends the bound once.
func TestDistillDropsRestatements(t *testing.T) {
	at := testNow.Add(-time.Hour)
	facts := []factview.Fact{
		testFact("conv-1", "fact-long", "Tim loves fantasy and feels his passion for fantasy brings him closer to people from all over the world.", []string{"tim"}, at),
		testFact("conv-1", "fact-short", "Tim loves fantasy.", []string{"tim"}, at.Add(-time.Hour)),
	}
	entries := Distill(facts, nil, Config{})
	if len(entries) != 1 {
		t.Fatalf("Distill returned %d entries, want the restatement dropped:\n%+v", len(entries), entries)
	}
	if entries[0].Provenance[0] != "fact-long" {
		t.Errorf("kept %v, want the higher-ranked line", entries[0].Provenance)
	}
}

// TestDistillLeavesSupersededFactsOut pins the maintenance hand-off: the older
// side of a supersede finding states what used to hold, so the block does not.
func TestDistillLeavesSupersededFactsOut(t *testing.T) {
	at := testNow.Add(-time.Hour)
	facts := []factview.Fact{
		testFact("conv-1", "fact-old", "Caroline loves painting.", []string{"caroline"}, at.Add(-48*time.Hour)),
		testFact("conv-1", "fact-new", "Caroline loves pottery class.", []string{"caroline"}, at),
	}
	entries := Distill(facts, map[string]struct{}{"fact-old": {}}, Config{})
	if len(entries) != 1 || entries[0].Provenance[0] != "fact-new" {
		t.Fatalf("Distill = %+v, want only the newer fact", entries)
	}
}

// TestDistillStopsAtTheBound pins the hard bound and the fill order: entries
// that fit are kept, one that does not is skipped rather than ending the pass.
func TestDistillStopsAtTheBound(t *testing.T) {
	at := testNow.Add(-time.Hour)
	long := "Melanie loves pottery and finds it relaxing and creative, and she has been making it for years."
	facts := []factview.Fact{
		testFact("conv-1", "fact-long", long, []string{"melanie"}, at),
		testFact("conv-1", "fact-short-1", "Melanie loves live music.", []string{"melanie"}, at.Add(-time.Hour)),
		testFact("conv-1", "fact-short-2", "Melanie loves camping trips.", []string{"melanie"}, at.Add(-2*time.Hour)),
	}
	budget := 15
	entries := Distill(facts, nil, Config{MaxTokens: budget})
	if len(entries) != 2 {
		t.Fatalf("Distill returned %d entries, want the two that fit:\n%+v", len(entries), entries)
	}
	if got := entriesTokens(entries); got > budget {
		t.Fatalf("block costs %d tokens, over the %d bound", got, budget)
	}
	if entries[0].Text != "Melanie loves live music." {
		t.Errorf("block starts with %q, want the highest-ranked line that fits", entries[0].Text)
	}
	byCount := Distill(facts, nil, Config{MaxEntries: 1})
	if len(byCount) != 1 {
		t.Fatalf("MaxEntries was not enforced: %+v", byCount)
	}
}

// TestDistillVocabularyIsConfigurable pins both ends of the knob: an empty
// vocabulary judges nothing, and a replacement vocabulary judges its own class.
func TestDistillVocabularyIsConfigurable(t *testing.T) {
	at := testNow.Add(-time.Hour)
	facts := []factview.Fact{
		testFact("conv-1", "fact-1", "Caroline loves pottery.", []string{"caroline"}, at),
		testFact("conv-1", "fact-2", "Caroline is a counselor.", []string{"caroline"}, at),
	}
	if entries := Distill(facts, nil, Config{Classes: []Class{}}); len(entries) != 0 {
		t.Fatalf("an empty vocabulary judged %+v", entries)
	}
	custom := []Class{{Name: "role", Phrases: []string{"is a counselor"}}}
	entries := Distill(facts, nil, Config{Classes: custom})
	if len(entries) != 1 || entries[0].Text != "Caroline is a counselor." {
		t.Fatalf("custom vocabulary = %+v, want the counselor line", entries)
	}
}

// TestDistillIsDeterministic pins that the same facts and policy produce the
// same block: the pass is diffed, re-run and compared.
func TestDistillIsDeterministic(t *testing.T) {
	at := testNow.Add(-time.Hour)
	facts := []factview.Fact{
		testFact("conv-1", "fact-b", "Caroline loves pottery.", []string{"caroline"}, at),
		testFact("conv-1", "fact-a", "Caroline loves live music.", []string{"caroline"}, at),
		testFact("conv-1", "fact-c", "Melanie is an artist.", []string{"melanie"}, at),
	}
	first := Distill(facts, nil, Config{})
	shuffled := []factview.Fact{facts[2], facts[0], facts[1]}
	second := Distill(shuffled, nil, Config{})
	if len(first) != len(second) {
		t.Fatalf("entry counts differ: %d vs %d", len(first), len(second))
	}
	for index := range first {
		if first[index].ID != second[index].ID || first[index].Text != second[index].Text ||
			strings.Join(first[index].Provenance, ",") != strings.Join(second[index].Provenance, ",") {
			t.Fatalf("entry %d differs: %+v vs %+v", index, first[index], second[index])
		}
	}
}

// TestClassifyReadsTheMeasuredShapes pins the matcher itself: the shapes it
// accepts, the reported cases it has to see through, and the near misses it has
// to leave alone.
func TestClassifyReadsTheMeasuredShapes(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		entities []string
		want     string
	}{
		{"direct preference", "Melanie loves pottery.", []string{"melanie"}, "preference"},
		{"reported preference", "Caroline said she loves creating art.", []string{"caroline"}, "preference"},
		{"possessive goal", "Melanie's goal is to open her own studio.", []string{"melanie"}, "goal"},
		{"copula identity", "Caroline is an artist.", []string{"caroline"}, "identity"},
		{"reported copula", "Joanna said she is a big fan of nature.", []string{"joanna"}, "identity"},
		{"habit", "Caroline plays acoustic guitar.", []string{"caroline"}, "habit"},
		{"two entities", "Caroline praised Melanie's painting.", []string{"caroline", "melanie"}, ""},
		{"event", "Caroline attended a workshop.", []string{"caroline"}, ""},
		{"noun usage", "Tim expressed that love is truly magical.", []string{"tim"}, ""},
		{"subject absent", "The pair agreed to meet soon.", []string{"caroline"}, ""},
		{"no vocabulary", "Caroline loves pottery.", []string{"caroline"}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classes := DefaultClasses()
			if test.name == "no vocabulary" {
				classes = nil
			}
			got, ok := Classify(test.text, test.entities, classes)
			if test.want == "" {
				if ok {
					t.Fatalf("Classify(%q) = %q, want no class", test.text, got)
				}
				return
			}
			if !ok || got != test.want {
				t.Fatalf("Classify(%q) = %q/%t, want %q", test.text, got, ok, test.want)
			}
		})
	}
}
