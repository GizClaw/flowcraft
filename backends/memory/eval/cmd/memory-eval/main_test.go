package main

import (
	"flag"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
)

// -categories serves two numbering schemes: LoCoMo numbers its categories and
// LongMemEval names them. A filter that only accepted numbers would make the
// LongMemEval question types unreachable from the command line, and one that
// only accepted names would break every recorded LoCoMo reading.
func TestParseCategoryFilterAcceptsIDsAndLabels(t *testing.T) {
	ids, labels, err := parseCategoryFilter("101, temporal-reasoning ,single-session-preference")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ids[101]; !ok || len(ids) != 1 {
		t.Fatalf("ids = %#v", ids)
	}
	if _, ok := labels["temporal-reasoning"]; !ok {
		t.Fatalf("labels = %#v", labels)
	}
	if _, ok := labels["single-session-preference"]; !ok {
		t.Fatalf("labels = %#v", labels)
	}
}

func TestParseCategoryFilterRejectsAnEmptyFilter(t *testing.T) {
	if _, _, err := parseCategoryFilter(" , "); err == nil {
		t.Fatal("an empty filter was accepted")
	}
}

// A question matches by its number or by its label, and the label comparison is
// case-insensitive because the flag is typed by hand.
func TestCategoryMatchesByIDOrLabel(t *testing.T) {
	ids := map[int]struct{}{eval.LongMemEvalTemporalReasoning: {}}
	labels := map[string]struct{}{"knowledge-update": {}}
	temporal := eval.Question{Category: eval.LongMemEvalTemporalReasoning, CategoryLabel: "temporal-reasoning"}
	update := eval.Question{Category: eval.LongMemEvalKnowledgeUpdate, CategoryLabel: "Knowledge-Update"}
	other := eval.Question{Category: 1, CategoryLabel: "single-hop"}
	if !categoryMatches(temporal, ids, labels) || !categoryMatches(update, ids, labels) {
		t.Fatal("a selected category did not match")
	}
	if categoryMatches(other, ids, labels) {
		t.Fatal("an unselected category matched")
	}
}

// The fingerprint has to keep saying "locomo10" for the dataset it has always
// named, or every stored report stops comparing; the images key only means
// something for the conversion that can attach images.
func TestFingerprintNamesTheConversion(t *testing.T) {
	if got := loaderFingerprint("locomo"); got != "locomo10" {
		t.Fatalf("locomo loader fingerprint = %q", got)
	}
	if got := loaderFingerprint("longmemeval"); got != "longmemeval" {
		t.Fatalf("longmemeval loader fingerprint = %q", got)
	}
	if got := imageFingerprint("longmemeval", "native"); got != "n/a" {
		t.Fatalf("longmemeval image fingerprint = %q", got)
	}
	if got := imageFingerprint("locomo", "native"); got != "native" {
		t.Fatalf("locomo image fingerprint = %q", got)
	}
}

// The per-loader judge default only applies to a flag the command line did not
// name: an explicit -judge-style is how one library is graded by both
// protocols.
func TestFlagWasSetTellsExplicitFromDefault(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.String("judge-style", "strict", "")
	flags.String("loader", "locomo", "")
	if err := flags.Parse([]string{"-judge-style", "lme"}); err != nil {
		t.Fatal(err)
	}
	if !flagWasSet(flags, "judge-style") {
		t.Fatal("an explicit flag was not seen")
	}
	if flagWasSet(flags, "loader") {
		t.Fatal("a defaulted flag was reported as explicit")
	}
}
