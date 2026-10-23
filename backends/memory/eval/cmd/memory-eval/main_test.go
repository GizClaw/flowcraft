package main

import (
	"flag"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	evalanswer "github.com/GizClaw/flowcraft/backends/memory/eval/answer"
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

// "both" is the two-column mode, and the strict column has to be the dataset's
// own rubric: a LongMemEval gold is a full clause, and 60 of its 500 rows are
// graded on a preference or on a refusal the strict fact rubric cannot see. A
// pair that hardcodes strict would grade those rows as misses in one column and
// report the resulting rate as if it measured the store.
func TestResolveJudgePlanPairsTheDatasetsOwnRubric(t *testing.T) {
	for _, test := range []struct {
		loader  string
		style   string
		primary evalanswer.JudgeStyle
		lenient evalanswer.JudgeStyle
	}{
		{loader: "locomo", style: "strict", primary: evalanswer.JudgeStrict},
		{loader: "locomo", style: "locomo", primary: evalanswer.JudgeLocoMo},
		{loader: "locomo", style: "lme", primary: evalanswer.JudgeLongMemEval},
		{loader: "locomo", style: "both", primary: evalanswer.JudgeStrict, lenient: evalanswer.JudgeLocoMo},
		{loader: "longmemeval", style: "lme", primary: evalanswer.JudgeLongMemEval},
		{loader: "longmemeval", style: "strict", primary: evalanswer.JudgeStrict},
		{loader: "longmemeval", style: "both", primary: evalanswer.JudgeLongMemEval, lenient: evalanswer.JudgeLocoMo},
		{loader: "longmemeval", style: "none"},
	} {
		plan, err := resolveJudgePlan(test.loader, test.style)
		if err != nil {
			t.Fatalf("%s/%s: %v", test.loader, test.style, err)
		}
		if plan.primary != test.primary || plan.lenient != test.lenient {
			t.Fatalf("%s/%s resolved to %q + %q, want %q + %q",
				test.loader, test.style, plan.primary, plan.lenient, test.primary, test.lenient)
		}
	}
	if _, err := resolveJudgePlan("longmemeval", "bogus"); err == nil {
		t.Fatal("an unknown judge style was accepted")
	}
}

// The fingerprint names the rubric that scored the answer columns. A pair's
// first half decides it, and the values reports were measured under stay put:
// LoCoMo's two-column runs and every no-judge (official-metric) run keep naming
// judge-v2.
func TestJudgePromptVersionNamesThePrimaryRubric(t *testing.T) {
	for _, test := range []struct {
		loader string
		style  string
		want   string
	}{
		{loader: "locomo", style: "both", want: evalanswer.JudgePromptVersion},
		{loader: "locomo", style: "none", want: evalanswer.JudgePromptVersion},
		{loader: "longmemeval", style: "both", want: evalanswer.JudgeLongMemEvalPromptVersion},
		{loader: "longmemeval", style: "lme", want: evalanswer.JudgeLongMemEvalPromptVersion},
		{loader: "longmemeval", style: "strict", want: evalanswer.JudgePromptVersion},
	} {
		plan, err := resolveJudgePlan(test.loader, test.style)
		if err != nil {
			t.Fatalf("%s/%s: %v", test.loader, test.style, err)
		}
		if got := plan.promptVersion(); got != test.want {
			t.Fatalf("%s/%s prompt version = %q, want %q", test.loader, test.style, got, test.want)
		}
	}
}

// A two-column run prints one table for the run and one line per category. The
// per-category merge is where the second column can silently disappear: the
// scenario lines would still carry it, so nothing but a test notices.
func TestMergeCategoriesKeepsTheLenientColumn(t *testing.T) {
	into := map[int]eval.CategoryStats{
		eval.LongMemEvalMultiSession: {Label: "multi-session", Questions: 2, Hits: 1, Answered: 2, AnswerHits: 1},
	}
	mergeCategories(into, map[int]eval.CategoryStats{
		eval.LongMemEvalMultiSession: {Label: "multi-session", Questions: 3, Hits: 2, Answered: 3, AnswerHits: 2,
			LenientAnswered: 3, LenientAnswerHits: 3},
		eval.LongMemEvalTemporalReasoning: {Label: "temporal-reasoning", Questions: 4, Hits: 4, Answered: 4, AnswerHits: 4,
			LenientAnswered: 4, LenientAnswerHits: 4},
	})
	merged := into[eval.LongMemEvalMultiSession]
	if merged.Questions != 5 || merged.Hits != 3 || merged.Answered != 5 || merged.AnswerHits != 3 {
		t.Fatalf("strict halves did not add up: %+v", merged)
	}
	if merged.LenientAnswered != 3 || merged.LenientAnswerHits != 3 {
		t.Fatalf("lenient column lost in the merge: %+v", merged)
	}
	if into[eval.LongMemEvalTemporalReasoning].LenientAnswerHits != 4 {
		t.Fatalf("a newly seen category lost its lenient column: %+v", into[eval.LongMemEvalTemporalReasoning])
	}
}
