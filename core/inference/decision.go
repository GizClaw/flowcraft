package inference

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// Decider is the judgment seam: one input, a batch of typed questions, an answer
// per question.
//
// It is not [Provider].Generate with a schema. A decision carries several
// questions in one call, answers that are typed (a predicate probability, a
// choice, a score), per-answer confidence and distributions, and per-question
// refusals — none of which is a message. The response shape is what earns a
// seam.
type Decider interface {
	// Decide answers every question in request order.
	Decide(ctx context.Context, request DecisionRequest) (DecisionResponse, error)
}

// QuestionKind names the shape of one answer.
type QuestionKind string

const (
	// QuestionPredicate asks a yes/no question; the answer carries P(true).
	QuestionPredicate QuestionKind = "predicate"
	// QuestionChoice asks to pick one of the declared choices.
	QuestionChoice QuestionKind = "choice"
	// QuestionScore asks for a value on a declared scale.
	QuestionScore QuestionKind = "score"
)

func (k QuestionKind) Validate() error {
	switch k {
	case QuestionPredicate, QuestionChoice, QuestionScore:
		return nil
	default:
		return fmt.Errorf("unknown question kind %q", k)
	}
}

// ChoiceValue is one selectable option. The value is typed: a string choice and
// a boolean choice are distinct even when their text reads the same, which is
// how the endpoint distinguishes them.
type ChoiceValue struct {
	// Label is what the caller sees and what an answer echoes.
	Label string `json:"label"`
	// Bool, when set, makes this a boolean choice; otherwise it is a string
	// choice with Label as its value.
	Bool *bool `json:"bool,omitempty"`
}

// Question is one question in a decision call.
type Question struct {
	// Name identifies the question; answers echo it, and a refusal names it.
	Name string `json:"name"`
	// Kind selects the answer shape.
	Kind QuestionKind `json:"kind"`
	// Instructions state what to judge.
	Instructions string `json:"instructions"`
	// Choices are the selectable options for QuestionChoice: at least two.
	Choices []ChoiceValue `json:"choices,omitempty"`
	// Levels are the scale for QuestionScore, lowest first.
	Levels []string `json:"levels,omitempty"`
}

// Validate rejects a question the contract cannot express.
func (q Question) Validate() error {
	if q.Name == "" {
		return fmt.Errorf("question name is required: answers and refusals are matched by name")
	}
	if err := q.Kind.Validate(); err != nil {
		return err
	}
	if q.Instructions == "" {
		return fmt.Errorf("question %q has no instructions", q.Name)
	}
	switch q.Kind {
	case QuestionChoice:
		if len(q.Choices) < 2 {
			return fmt.Errorf("question %q needs at least two choices", q.Name)
		}
		if len(q.Choices) > 255 {
			return fmt.Errorf("question %q has %d choices, the limit is 255", q.Name, len(q.Choices))
		}
	case QuestionScore:
		if len(q.Levels) < 2 {
			return fmt.Errorf("question %q needs at least two score levels", q.Name)
		}
	default:
		if len(q.Choices) > 0 || len(q.Levels) > 0 {
			return fmt.Errorf("question %q: choices and levels apply to choice and score questions", q.Name)
		}
	}
	return nil
}

// DecisionRequest is one judgment call.
type DecisionRequest struct {
	// Model names the provider-qualified model that should judge.
	Model model.ModelRef `json:"model"`
	// Input is the material being judged: text, or a message whose parts carry
	// what the questions are about.
	Input message.Content `json:"input"`
	// Questions are answered in order; empty is rejected.
	Questions  []Question `json:"questions"`
	Extensions Extensions `json:"-"`
}

// Validate rejects a request before any provider I/O.
func (r DecisionRequest) Validate() error {
	if err := r.Model.Validate(); err != nil {
		return inferenceErr(InvalidRequest, FieldModel, err)
	}
	if err := r.Input.Validate(); err != nil {
		return inferenceErr(InvalidRequest, FieldInput, err)
	}
	if len(r.Questions) == 0 {
		return inferenceErr(InvalidRequest, FieldQuestions, fmt.Errorf("at least one question is required"))
	}
	seen := make(map[string]bool, len(r.Questions))
	for i, question := range r.Questions {
		if err := question.Validate(); err != nil {
			return inferenceErr(InvalidRequest, FieldQuestions, fmt.Errorf("question %d: %w", i, err))
		}
		if seen[question.Name] {
			return inferenceErr(InvalidRequest, FieldQuestions,
				fmt.Errorf("question name %q appears twice: answers are matched by name", question.Name))
		}
		seen[question.Name] = true
	}
	if err := r.Extensions.Validate(); err != nil {
		return inferenceErr(InvalidExtension, FieldExtensions, err)
	}
	return nil
}

// ActiveFields names the knobs this request uses.
func (r DecisionRequest) ActiveFields() []Field {
	fields := []Field{FieldModel, FieldInput, FieldQuestions}
	return append(fields, r.Extensions.ActiveFields()...)
}

// AnswerKind names how one question was answered.
type AnswerKind string

const (
	// AnswerRefusal means the model declined this question. Other questions in
	// the same call may still be answered.
	AnswerRefusal AnswerKind = "refusal"
)

// Answer is one judgment.
type Answer struct {
	// Name echoes the question's name.
	Name string `json:"name"`
	// Kind matches the question's kind, or is AnswerRefusal.
	Kind AnswerKind `json:"kind"`
	// Probability is P(true) for a predicate question.
	Probability *float64 `json:"probability,omitempty"`
	// Choice is the selected option for a choice question.
	Choice *ChoiceValue `json:"choice,omitempty"`
	// Score is the value for a score question.
	Score *float64 `json:"score,omitempty"`
	// Confidence is the model's confidence in this answer.
	Confidence float64 `json:"confidence,omitempty"`
	// Probabilities is the distribution over the alternatives, when reported.
	Probabilities []Probability `json:"probabilities,omitempty"`
	// Refusal explains a declined question. Set only for AnswerRefusal.
	Refusal string `json:"refusal,omitempty"`
}

// Probability is one entry of a reported distribution.
type Probability struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// DecisionResponse is one batch of judgments.
type DecisionResponse struct {
	// Answers are in request order.
	Answers  []Answer    `json:"answers"`
	Usage    usage.Usage `json:"usage,omitzero"`
	Metadata Metadata    `json:"metadata,omitzero"`
	Warnings []Warning   `json:"warnings,omitempty"`
}

// ValidateFor reports whether the answer can be the answer to request.
//
// A refusal is a valid answer: it is per question, and the others still count.
// A kind that does not match its question is not valid — that answer belongs to
// a different question, and attributing it would silently corrupt the judgment.
func (r DecisionResponse) ValidateFor(request DecisionRequest) error {
	if len(r.Answers) != len(request.Questions) {
		return inferenceErr(InvalidProviderResponse, FieldAnswers,
			fmt.Errorf("got %d answers for %d questions", len(r.Answers), len(request.Questions)))
	}
	// Match by name, not by position: answers echo the question name, and a
	// provider that reorders them would otherwise have its answers attributed to
	// the wrong questions — silently, because the count still matches.
	byName := make(map[string]Question, len(request.Questions))
	for _, question := range request.Questions {
		byName[question.Name] = question
	}
	seen := make(map[string]bool, len(r.Answers))
	for i, answer := range r.Answers {
		question, ok := byName[answer.Name]
		if !ok {
			return inferenceErr(InvalidProviderResponse, FieldAnswers,
				fmt.Errorf("answer %d names %q, which no question asked", i, answer.Name))
		}
		if seen[answer.Name] {
			return inferenceErr(InvalidProviderResponse, FieldAnswers,
				fmt.Errorf("answer %q appears twice", answer.Name))
		}
		seen[answer.Name] = true

		// A refusal is a valid answer: it is per question, and the others still
		// count.
		if answer.Kind == AnswerRefusal {
			continue
		}
		if string(answer.Kind) != string(question.Kind) {
			return inferenceErr(InvalidProviderResponse, FieldAnswers,
				fmt.Errorf("answer %q has kind %q, want %q", answer.Name, answer.Kind, question.Kind))
		}
		switch question.Kind {
		case QuestionPredicate:
			if answer.Probability == nil || *answer.Probability < 0 || *answer.Probability > 1 {
				return inferenceErr(InvalidProviderResponse, FieldAnswers,
					fmt.Errorf("answer %q needs a probability in [0, 1]", answer.Name))
			}
		case QuestionChoice:
			if answer.Choice == nil {
				return inferenceErr(InvalidProviderResponse, FieldAnswers,
					fmt.Errorf("answer %q has no choice", answer.Name))
			}
			if !declares(question.Choices, *answer.Choice) {
				return inferenceErr(InvalidProviderResponse, FieldAnswers,
					fmt.Errorf("answer %q picked %q, which the question did not offer", answer.Name, answer.Choice.Label))
			}
		case QuestionScore:
			if answer.Score == nil {
				return inferenceErr(InvalidProviderResponse, FieldAnswers,
					fmt.Errorf("answer %q has no score", answer.Name))
			}
		}
	}
	return nil
}

// declares reports whether value is one of the offered choices, comparing the
// typed value rather than its label.
func declares(choices []ChoiceValue, value ChoiceValue) bool {
	for _, choice := range choices {
		if choice.Label != value.Label {
			continue
		}
		if (choice.Bool == nil) != (value.Bool == nil) {
			continue
		}
		if choice.Bool == nil || *choice.Bool == *value.Bool {
			return true
		}
	}
	return false
}
