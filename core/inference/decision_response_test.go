package inference

import (
	"math"
	"strings"
	"testing"
)

func probability(value float64) *float64 {
	return &value
}

func TestDecisionAnswerValidate(t *testing.T) {
	stringChoice := NewDecisionStringChoice("yes")
	booleanChoice := NewDecisionBooleanChoice(true)
	cases := []struct {
		name   string
		answer DecisionAnswer
		want   string
	}{
		{
			name: "predicate without probability",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerPredicate,
			},
			want: "requires a probability",
		},
		{
			name: "predicate probability above one",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerPredicate, Probability: probability(1.5),
			},
			want: "outside [0,1]",
		},
		{
			name: "predicate with confidence",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerPredicate,
				Probability: probability(0.5), Confidence: probability(0.5),
			},
			want: "fields of another kind",
		},
		{
			name: "choice without the chosen value",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerChoice,
				Confidence: probability(0.5),
				Probabilities: []DecisionOptionProbability{
					{Value: stringChoice, Probability: 0.5},
				},
			},
			want: "requires the chosen value",
		},
		{
			name: "choice whose value is not in its distribution",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerChoice,
				Choice:     &booleanChoice,
				Confidence: probability(0.5),
				Probabilities: []DecisionOptionProbability{
					{Value: stringChoice, Probability: 1},
				},
			},
			want: "is not part of its distribution",
		},
		{
			name: "choice without confidence",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerChoice,
				Choice: &stringChoice,
				Probabilities: []DecisionOptionProbability{
					{Value: stringChoice, Probability: 1},
				},
			},
			want: "requires a confidence",
		},
		{
			name: "choice repeating an option",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerChoice,
				Choice:     &stringChoice,
				Confidence: probability(0.5),
				Probabilities: []DecisionOptionProbability{
					{Value: stringChoice, Probability: 0.5},
					{Value: stringChoice, Probability: 0.5},
				},
			},
			want: "repeats option",
		},
		{
			name: "score without a score",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerScore,
			},
			want: "requires a score",
		},
		{
			name: "score that is not finite",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerScore, Score: probability(math.NaN()),
			},
			want: "is not finite",
		},
		{
			name: "score without levels",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerScore, Score: probability(0.5),
			},
			want: "requires the level distribution",
		},
		{
			name: "score repeating a level",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerScore, Score: probability(0.5),
				Confidence: probability(0.5),
				Levels: []DecisionLevelProbability{
					{Level: 0, Probability: 0.5},
					{Level: 0, Probability: 0.5},
				},
			},
			want: "repeats level",
		},
		{
			name: "refusal with a probability",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerRefusal, Probability: probability(0.5),
			},
			want: "fields of another kind",
		},
		{
			name:   "answer without a name",
			answer: DecisionAnswer{Kind: DecisionAnswerRefusal},
			want:   "name is required",
		},
		{
			name: "unknown kind",
			answer: DecisionAnswer{
				Name: "q", Kind: DecisionAnswerKind("noul"),
			},
			want: `unknown decision answer kind "noul"`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.answer.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Validate() = %v, want substring %q", err, testCase.want)
			}
		})
	}
}

// decisionResponseFixture answers decisionRequestFixture in question order.
func decisionResponseFixture() DecisionResponse {
	stringChoice := NewDecisionStringChoice("yes")
	return DecisionResponse{
		Answers: []DecisionAnswer{
			{
				Name:        "is_true",
				Kind:        DecisionAnswerPredicate,
				Probability: probability(0.75),
			},
			{
				Name:       "which",
				Kind:       DecisionAnswerChoice,
				Choice:     &stringChoice,
				Confidence: probability(0.6),
				Probabilities: []DecisionOptionProbability{
					{Value: stringChoice, Probability: 0.6},
					{Value: NewDecisionBooleanChoice(true), Probability: 0.4},
				},
			},
			{
				Name:       "grade",
				Kind:       DecisionAnswerScore,
				Score:      probability(0.25),
				Confidence: probability(0.5),
				Levels: []DecisionLevelProbability{
					{Level: 0, Label: "low", Probability: 0.5},
					{Level: 1, Label: "high", Probability: 0.5},
				},
			},
		},
	}
}

func TestDecisionResponseValidateFor(t *testing.T) {
	request := decisionRequestFixture()
	response := decisionResponseFixture()
	if err := response.ValidateFor(request); err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*DecisionResponse)
		want   string
	}{
		{
			name: "answer count",
			mutate: func(r *DecisionResponse) {
				r.Answers = r.Answers[:2]
			},
			want: "does not match question count",
		},
		{
			name: "answers out of order",
			mutate: func(r *DecisionResponse) {
				r.Answers[0], r.Answers[1] = r.Answers[1], r.Answers[0]
			},
			want: `answer names "which", want "is_true"`,
		},
		{
			name: "kind does not match the question",
			mutate: func(r *DecisionResponse) {
				stringChoice := NewDecisionStringChoice("yes")
				r.Answers[0] = DecisionAnswer{
					Name:       "is_true",
					Kind:       DecisionAnswerChoice,
					Choice:     &stringChoice,
					Confidence: probability(0.6),
					Probabilities: []DecisionOptionProbability{
						{Value: stringChoice, Probability: 0.6},
						{Value: NewDecisionBooleanChoice(true), Probability: 0.4},
					},
				}
			},
			want: "does not match question kind",
		},
		{
			name: "level outside the question",
			mutate: func(r *DecisionResponse) {
				r.Answers[2].Levels[1].Level = 5
			},
			want: "but the question has 2 levels",
		},
		{
			name: "level label outside the question",
			mutate: func(r *DecisionResponse) {
				r.Answers[2].Levels[1].Label = "medium"
			},
			want: `is labelled "medium", want "high"`,
		},
		{
			name: "choice outside the question's options",
			mutate: func(r *DecisionResponse) {
				unknown := NewDecisionStringChoice("maybe")
				r.Answers[1].Choice = &unknown
				r.Answers[1].Probabilities = []DecisionOptionProbability{
					{Value: unknown, Probability: 1},
				}
			},
			want: `choice answer "maybe" is not one of the question's options`,
		},
		{
			name: "choice distribution value outside the question's options",
			mutate: func(r *DecisionResponse) {
				// The question offers the boolean true; a driver mapping the
				// legend onto the string "true" must not ship silently.
				r.Answers[1].Probabilities[1].Value =
					NewDecisionStringChoice("true")
			},
			want: `choice answer option 1 "true" is not one of the question's options`,
		},
		{
			name: "score above the question's levels",
			mutate: func(r *DecisionResponse) {
				*r.Answers[2].Score = 42
			},
			want: "outside the question's level range [0, 1]",
		},
		{
			name: "score below the question's levels",
			mutate: func(r *DecisionResponse) {
				*r.Answers[2].Score = -0.5
			},
			want: "outside the question's level range [0, 1]",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := response.Clone()
			testCase.mutate(&mutated)
			err := mutated.ValidateFor(request)
			if err == nil {
				t.Fatalf("ValidateFor() = nil, want error")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("ValidateFor() = %v, want substring %q", err, testCase.want)
			}
		})
	}
}

// TestDecisionResponseAcceptsRefusal pins that a refusal answers any question
// kind without a position fallback: the name still locates it, and the other
// answers are untouched.
func TestDecisionResponseAcceptsRefusal(t *testing.T) {
	request := decisionRequestFixture()
	response := decisionResponseFixture()
	response.Answers[2] = DecisionAnswer{
		Name: "grade",
		Kind: DecisionAnswerRefusal,
	}
	if err := response.ValidateFor(request); err != nil {
		t.Fatalf("refusal was rejected: %v", err)
	}
}

func TestDecisionResponseCloneIsIndependent(t *testing.T) {
	response := decisionResponseFixture()
	clone := response.Clone()
	clone.Answers[1].Probabilities[0].Probability = 0.1
	*clone.Answers[1].Choice = NewDecisionStringChoice("mutated")
	clone.Answers[2].Levels[0].Label = "mutated"

	if response.Answers[1].Probabilities[0].Probability != 0.6 {
		t.Fatal("Clone aliases the option distribution")
	}
	if response.Answers[1].Choice.String != "yes" {
		t.Fatal("Clone aliases the chosen value")
	}
	if response.Answers[2].Levels[0].Label != "low" {
		t.Fatal("Clone aliases the level distribution")
	}
}
