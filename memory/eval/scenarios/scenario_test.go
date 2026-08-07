package scenarios

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/eval/dataset"
)

func TestLookupScenario(t *testing.T) {
	if _, err := Lookup("locomo"); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup("longmemeval"); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup("unknown"); err == nil {
		t.Fatal("unknown scenario should fail")
	}
}

func TestAbstentionScoring(t *testing.T) {
	scenario := longmemevalScenario{}
	absQuestion := dataset.Question{
		GoldAnswers: []string{"(no answer)"},
		Tags:        []string{"qtype:knowledge-update", "abs"},
	}
	normalQuestion := dataset.Question{
		GoldAnswers: []string{"Paris"},
		Tags:        []string{"qtype:single-session-user"},
	}

	_, _, _, abstention := scenario.Score("I don't know", absQuestion, 0, false)
	if abstention == nil || *abstention != 1 {
		t.Fatalf("correct abstention = %v", abstention)
	}
	_, _, _, abstention = scenario.Score("The user lives in Paris.", absQuestion, 0, false)
	if abstention == nil || *abstention != 0 {
		t.Fatalf("wrong abstention = %v", abstention)
	}
	_, _, _, abstention = scenario.Score("I don't know", normalQuestion, 0, false)
	if abstention != nil {
		t.Fatalf("non-abstention question should not score abstention: %v", abstention)
	}

	aggregate := scenario.Aggregate([]QuestionScore{
		{Abstention: boolFloatPtr(true)},
		{Abstention: boolFloatPtr(false)},
		{Abstention: nil},
	})
	if aggregate.Abstain == nil || *aggregate.Abstain != 0.5 {
		t.Fatalf("qa.abstain = %v", aggregate.Abstain)
	}
}
