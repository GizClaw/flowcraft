package scenarios

import (
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/memory/eval/dataset"
)

// Scenario encapsulates one eval benchmark: identity, upstream conversion,
// scoring policy, and result aggregation. The ingest/derive/recall pipeline
// is shared; only these two seams differ between scenarios.
type Scenario interface {
	Name() string
	RuntimeID() string
	Convert(raw []byte) (dataset.Dataset, Stats, error)
	// Score returns QA metrics for one prediction. abstention is nil when
	// the scenario/question has no abstention semantics.
	Score(prediction string, question dataset.Question, judge float64, judgeScored bool) (em, f1, itemRecall float64, abstention *float64)
	Aggregate(scores []QuestionScore) ScoreAggregate
}

var registry = map[string]Scenario{
	"locomo":      locomoScenario{},
	"longmemeval": longmemevalScenario{},
}

// Lookup resolves a scenario by its CLI/workflow name.
func Lookup(name string) (Scenario, error) {
	scenario, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown scenario %q (want locomo or longmemeval)", name)
	}
	return scenario, nil
}

// isAbstentionQuestion reports whether the question expects the model to
// decline answering (LongMemEval `_abs` instances with no gold answer).
func isAbstentionQuestion(question dataset.Question) bool {
	return slices.Contains(question.Tags, "abs")
}

// hasAbstained detects a refusal / no-information answer.
func hasAbstained(prediction string) bool {
	text := normalizeText(prediction)
	markers := []string{
		"i don't know", "i do not know", "no information", "not mentioned",
		"cannot answer", "can't answer", "not enough information",
		"not available", "unknown", "no answer",
	}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
