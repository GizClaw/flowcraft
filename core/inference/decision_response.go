package inference

import (
	"fmt"
	"math"
)

// DecisionAnswerKind is the kind of one answer. Refusal is not a question
// kind: it replaces the answer to any question the model declined.
type DecisionAnswerKind string

const (
	DecisionAnswerPredicate DecisionAnswerKind = "predicate"
	DecisionAnswerChoice    DecisionAnswerKind = "choice"
	DecisionAnswerScore     DecisionAnswerKind = "score"
	// DecisionAnswerRefusal means the model declined this question. Other
	// questions in the same response are still answered.
	DecisionAnswerRefusal DecisionAnswerKind = "refusal"
)

func (k DecisionAnswerKind) Validate() error {
	switch k {
	case DecisionAnswerPredicate, DecisionAnswerChoice,
		DecisionAnswerScore, DecisionAnswerRefusal:
		return nil
	}
	return fmt.Errorf("unknown decision answer kind %q", k)
}

// DecisionOptionProbability is one choice option's probability. The
// distribution keeps the typed value, so string and boolean options stay
// distinct here too.
type DecisionOptionProbability struct {
	Value       DecisionChoiceValue `json:"value"`
	Probability float64             `json:"probability"`
}

// DecisionLevelProbability is one score level's probability. Level is the
// 0-based index into the question's levels; Label repeats the request's level
// label so the response reads on its own.
type DecisionLevelProbability struct {
	Level       int     `json:"level"`
	Label       string  `json:"label"`
	Probability float64 `json:"probability"`
}

// DecisionAnswer is the answer to one question. Which fields are populated
// follows Kind: predicate carries Probability; choice carries Choice,
// Probabilities, and Confidence; score carries Score — the probability-
// weighted value across the question's levels, so it stays inside
// [0, len(levels)-1] — Levels, and Confidence; refusal carries only Name. A
// predicate has no confidence on any wire: that derived value exists for
// distributions, and the canonical layer never mints one for a single
// probability.
type DecisionAnswer struct {
	Name          string                      `json:"name"`
	Kind          DecisionAnswerKind          `json:"kind"`
	Probability   *float64                    `json:"probability,omitempty"`
	Choice        *DecisionChoiceValue        `json:"choice,omitempty"`
	Confidence    *float64                    `json:"confidence,omitempty"`
	Probabilities []DecisionOptionProbability `json:"probabilities,omitempty"`
	Score         *float64                    `json:"score,omitempty"`
	Levels        []DecisionLevelProbability  `json:"levels,omitempty"`
}

func (a DecisionAnswer) Clone() DecisionAnswer {
	clone := a
	clone.Probability = cloneFloat(a.Probability)
	clone.Confidence = cloneFloat(a.Confidence)
	clone.Score = cloneFloat(a.Score)
	clone.Probabilities = append(
		[]DecisionOptionProbability(nil), a.Probabilities...)
	clone.Levels = append([]DecisionLevelProbability(nil), a.Levels...)
	if a.Choice != nil {
		choice := *a.Choice
		clone.Choice = &choice
	}
	return clone
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

// Validate checks that the answer's populated fields match its kind and that
// every probability is a real probability in [0,1].
func (a DecisionAnswer) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("decision answer name is required")
	}
	if err := a.Kind.Validate(); err != nil {
		return err
	}
	switch a.Kind {
	case DecisionAnswerPredicate:
		return a.validatePredicate()
	case DecisionAnswerChoice:
		return a.validateChoice()
	case DecisionAnswerScore:
		return a.validateScore()
	case DecisionAnswerRefusal:
		return a.validateRefusal()
	}
	return nil
}

func (a DecisionAnswer) validatePredicate() error {
	if a.Probability == nil {
		return fmt.Errorf("predicate answer requires a probability")
	}
	if err := validateDecisionProbability(*a.Probability); err != nil {
		return fmt.Errorf("predicate answer: %w", err)
	}
	if a.Choice != nil || a.Confidence != nil || a.Score != nil ||
		len(a.Probabilities) > 0 || len(a.Levels) > 0 {
		return fmt.Errorf("predicate answer carries fields of another kind")
	}
	return nil
}

func (a DecisionAnswer) validateChoice() error {
	if a.Choice == nil {
		return fmt.Errorf("choice answer requires the chosen value")
	}
	if err := a.Choice.Validate(); err != nil {
		return fmt.Errorf("choice answer: %w", err)
	}
	if a.Probability != nil || a.Score != nil || len(a.Levels) > 0 {
		return fmt.Errorf("choice answer carries fields of another kind")
	}
	if len(a.Probabilities) == 0 {
		return fmt.Errorf("choice answer requires the option distribution")
	}
	seen := make(map[DecisionChoiceValue]struct{}, len(a.Probabilities))
	chosen := false
	for index, option := range a.Probabilities {
		if err := option.Value.Validate(); err != nil {
			return fmt.Errorf("choice answer option %d: %w", index, err)
		}
		if err := validateDecisionProbability(option.Probability); err != nil {
			return fmt.Errorf("choice answer option %d: %w", index, err)
		}
		if _, ok := seen[option.Value]; ok {
			return fmt.Errorf(
				"choice answer repeats option %s", option.Value.Describe())
		}
		seen[option.Value] = struct{}{}
		if option.Value == *a.Choice {
			chosen = true
		}
	}
	if !chosen {
		return fmt.Errorf(
			"choice answer %s is not part of its distribution",
			a.Choice.Describe(),
		)
	}
	if a.Confidence == nil {
		return fmt.Errorf("choice answer requires a confidence")
	}
	if err := validateDecisionProbability(*a.Confidence); err != nil {
		return fmt.Errorf("choice answer confidence: %w", err)
	}
	return nil
}

func (a DecisionAnswer) validateScore() error {
	if a.Score == nil {
		return fmt.Errorf("score answer requires a score")
	}
	if math.IsNaN(*a.Score) || math.IsInf(*a.Score, 0) {
		return fmt.Errorf("score answer score is not finite")
	}
	if a.Probability != nil || a.Choice != nil || len(a.Probabilities) > 0 {
		return fmt.Errorf("score answer carries fields of another kind")
	}
	if len(a.Levels) == 0 {
		return fmt.Errorf("score answer requires the level distribution")
	}
	seen := make(map[int]struct{}, len(a.Levels))
	for index, level := range a.Levels {
		if level.Level < 0 {
			return fmt.Errorf("score answer level %d is negative", index)
		}
		if err := validateDecisionProbability(level.Probability); err != nil {
			return fmt.Errorf("score answer level %d: %w", index, err)
		}
		if _, ok := seen[level.Level]; ok {
			return fmt.Errorf(
				"score answer repeats level %d", level.Level)
		}
		seen[level.Level] = struct{}{}
	}
	if a.Confidence == nil {
		return fmt.Errorf("score answer requires a confidence")
	}
	if err := validateDecisionProbability(*a.Confidence); err != nil {
		return fmt.Errorf("score answer confidence: %w", err)
	}
	return nil
}

func (a DecisionAnswer) validateRefusal() error {
	if a.Probability != nil || a.Choice != nil || a.Confidence != nil ||
		a.Score != nil || len(a.Probabilities) > 0 || len(a.Levels) > 0 {
		return fmt.Errorf("refusal answer carries fields of another kind")
	}
	return nil
}

// validateDecisionProbability reports whether value is a probability. The
// canonical layer checks each value's range and never the distribution's sum:
// completeness is a wire property (a provider may legally report a rounded or
// partial distribution), so the sum check belongs to the driver that knows
// its own contract.
func validateDecisionProbability(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("probability %v is not finite", value)
	}
	if value < 0 || value > 1 {
		return fmt.Errorf("probability %v is outside [0,1]", value)
	}
	return nil
}

// ValidateFor checks the response against the request it answers: one answer
// per question in question order, each answer carrying its question's name,
// and each answer's kind matching the question's kind — except refusal, which
// may replace any question's answer. A typed answer is reconciled with the
// question's declared values: level indexes and labels, the score against the
// level range, and a choice answer's values — the chosen one and every
// distributed one — against the offered options.
func (a DecisionAnswer) ValidateFor(question DecisionQuestion) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.Name != question.Name {
		return fmt.Errorf("answer names %q, want %q", a.Name, question.Name)
	}
	if a.Kind == DecisionAnswerRefusal {
		return nil
	}
	want := DecisionAnswerKind(question.Kind)
	if a.Kind != want {
		return fmt.Errorf(
			"answer kind %q does not match question kind %q", a.Kind, want)
	}
	if a.Kind == DecisionAnswerChoice {
		return a.validateChoiceFor(question)
	}
	if a.Kind != DecisionAnswerScore {
		return nil
	}
	for _, level := range a.Levels {
		if level.Level >= len(question.Levels) {
			return fmt.Errorf(
				"answer names level %d, but the question has %d levels",
				level.Level,
				len(question.Levels),
			)
		}
		if level.Label != "" &&
			level.Label != question.Levels[level.Level].Label {
			return fmt.Errorf(
				"answer level %d is labelled %q, want %q",
				level.Level,
				level.Label,
				question.Levels[level.Level].Label,
			)
		}
	}
	if *a.Score < 0 || *a.Score > float64(len(question.Levels)-1) {
		return fmt.Errorf(
			"answer score %v is outside the question's level range [0, %d]",
			*a.Score,
			len(question.Levels)-1,
		)
	}
	return nil
}

// validateChoiceFor reconciles a choice answer with the question's option
// set: the chosen value and every distributed value must be one of the
// question's declared options. This is the checkpoint between a driver's
// legend mapping — where typed values get swapped or mislabelled, the string
// "true" for the boolean true among them — and every caller; distribution
// completeness and its sum stay driver-owned, as validateDecisionProbability
// documents.
func (a DecisionAnswer) validateChoiceFor(question DecisionQuestion) error {
	offered := make(map[DecisionChoiceValue]struct{}, len(question.Choices))
	for _, choice := range question.Choices {
		offered[choice.Value] = struct{}{}
	}
	if _, ok := offered[*a.Choice]; !ok {
		return fmt.Errorf(
			"choice answer %s is not one of the question's options",
			a.Choice.Describe(),
		)
	}
	for index, option := range a.Probabilities {
		if _, ok := offered[option.Value]; !ok {
			return fmt.Errorf(
				"choice answer option %d %s is not one of the question's options",
				index,
				option.Value.Describe(),
			)
		}
	}
	return nil
}

// DecisionResponse is the canonical decision result: one answer per question,
// in question order, plus the model identifier the wire reported (which may
// be a versioned id rather than the requested alias) and token usage.
type DecisionResponse struct {
	Answers  []DecisionAnswer `json:"answers"`
	Model    string           `json:"model,omitempty"`
	Usage    Usage            `json:"usage"`
	Metadata Metadata         `json:"metadata"`
}

func (r DecisionResponse) Clone() DecisionResponse {
	clone := r
	clone.Answers = make([]DecisionAnswer, len(r.Answers))
	for index, answer := range r.Answers {
		clone.Answers[index] = answer.Clone()
	}
	clone.Usage = r.Usage.Clone()
	clone.Metadata = r.Metadata.Clone()
	return clone
}

func (r DecisionResponse) ValidateFor(request DecisionRequest) error {
	if len(r.Answers) != len(request.Questions) {
		return fmt.Errorf(
			"decision answer count %d does not match question count %d",
			len(r.Answers),
			len(request.Questions),
		)
	}
	for index, answer := range r.Answers {
		question := request.Questions[index]
		if err := answer.ValidateFor(question); err != nil {
			return fmt.Errorf("decision answer %d: %w", index, err)
		}
	}
	return r.Usage.Validate()
}
