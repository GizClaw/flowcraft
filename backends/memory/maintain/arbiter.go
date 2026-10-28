package maintain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// Verdict is what one candidate pair is judged to be.
type Verdict string

const (
	// VerdictReplaced: the newer fact states a new value of a relation the
	// older one stated, so the older one is no longer the current value of
	// it. It is the only verdict that becomes a finding.
	VerdictReplaced Verdict = "replaced"
	// VerdictBothCurrent: the pair can hold at once -- a repeated event, a
	// preference, an addition, or the same value restated. Nothing is
	// superseded.
	VerdictBothCurrent Verdict = "both-current"
	// VerdictUndecided: no answer was made for this pair -- the arbiter
	// declined it, or its probability did not clear the arbiter's threshold.
	// An undecided pair leaves the older fact current: a missed update costs
	// one stale value, and a wrong one penalizes a fact that is still true.
	VerdictUndecided Verdict = "undecided"
)

// Judgment is one arbiter answer about one pair.
type Judgment struct {
	Verdict Verdict
	// Confidence is the probability the arbiter put on the verdict it chose.
	// For VerdictReplaced it is the probability that the older fact is no
	// longer current, which is what the finding records; for the other
	// verdicts it is the probability of that answer, and a pair answered
	// both-current below the threshold is still both-current -- the threshold
	// gates findings, not opinions.
	Confidence float64
}

// Pair is one candidate update: an older fact, and a later fact of the same
// conversation that may replace it. The pass orders the two by its own recency
// key, so Older is never the later of the pair.
//
// A pair is a question, not a finding: the pass proposes it because the two
// facts share a subject and no rule could decide between them, and the arbiter
// is what decides.
type Pair struct {
	Conversation string
	Older        factview.Fact
	Newer        factview.Fact
}

// Arbiter decides the candidate pairs the pass's own rules cannot: the pair
// whose predicates the vocabulary does not recognize, and whose texts share
// too few tokens for the similarity rule. Neither rule can see a Chinese
// update whose wording moved with it, and neither can see a preference that
// changed; a model reading the two statements can see both.
//
// It is an interface rather than a model call because the pass has to stay
// testable without a provider: a host wires a DecisionArbiter into
// Config.Arbiter, and a test wires a function that answers from a table.
type Arbiter interface {
	// Identity names the judgment this arbiter makes -- the model, the
	// question wording, the threshold -- and is digested into
	// ProducerVersion. It is the arbiter's counterpart to the vocabulary's
	// digest: an overlay written under one judgment is not served under
	// another, because the two would call the same fact current and stale.
	Identity() string
	// Adjudicate answers pairs in order: the returned slice is parallel to
	// the input, and a pair the arbiter did not answer is VerdictUndecided
	// rather than an error. An error means no answer was obtained at all and
	// the caller has no findings for these pairs.
	Adjudicate(ctx context.Context, pairs []Pair) ([]Judgment, error)
}

// Defaults of the decision arbiter. The threshold is deliberately above a coin
// flip: a finding penalizes a canonical fact that the pass cannot tell is
// right, and the read path applies it until the next pass, so the model has to
// be substantially sure. 200 questions is the OpenAI wire's own per-call
// limit.
const (
	defaultArbiterThreshold = 0.75
	defaultArbiterBatch     = 200
	// arbiterQuestionVersion names the wording below. It is in the arbiter's
	// identity, so rephrasing a question invalidates the overlays the earlier
	// wording wrote instead of serving them as this wording's judgment.
	arbiterQuestionVersion = "supersede-v1"
)

// arbiterInput frames the batch. It is the only thing the questions share: the
// surface answers each question from that question's own text, so a pair has to
// be stated there rather than in a shared evidence blob -- a batch that put
// thirteen pairs in one input and asked thirteen identical questions got one
// answer, repeated for every pair.
const arbiterInput = "Supersede check. Each question names two statements recorded about the same person " +
	"or thing, the older one first and the newer one second."

// arbiterInstructions is the question one pair is asked, in the terms the two
// rules fail on rather than in terms of a relation class: the arbiter is asked
// whether the older statement is still a current description of the world,
// which is what matters, and not which relation the two statements share, which
// is the vocabulary's vocabulary rather than the model's.
const arbiterInstructions = "Decide whether the newer statement makes the older one out of date. " +
	"Answer \"replaced\" when the two state the same single-valued fact about that subject -- where they live, " +
	"who employs them, what their job is, whether and whom they married, when and where they were born -- " +
	"and the newer statement gives a different value, so the older one is no longer true. " +
	"Answer \"both-current\" when both can be true at the same time: an event or an activity that happened more than once, " +
	"a preference, an addition (one more pet, one more child, one more job title held), a plan, " +
	"or a restatement that agrees with the older statement."

const (
	arbiterReplacedMeans = "The newer statement gives a new value of the same single-valued fact, so the older one is no longer true."
	arbiterBothMeans     = "Both statements can be true at once, or the newer one restates the older one."
)

// DecisionArbiter is the Arbiter the platform's decision workload answers: one
// call carries a batch of pairs as a JSON array and one choice question per
// pair, and the answers come back in question order. The workload is the
// platform's, not a bespoke prompt: the request is a DecisionRequest, the
// model is a ModelRef, and a provider that cannot serve the decision operation
// fails at the call rather than silently answering a generate request.
type DecisionArbiter struct {
	// Decisions is the decision surface. The inference assembly is one.
	Decisions DecisionSource
	// Model is the model the pass asks. It has to be declared with the
	// decision operation on its provider: the workload routes by what the
	// deployment says the model serves.
	Model model.ModelRef
	// Threshold is the probability of "replaced" a pair needs before the
	// finding is made (default 0.75).
	Threshold float64
	// Batch caps how many pairs one call carries (default 200, the OpenAI
	// wire's own limit).
	Batch int
}

// DecisionSource is the decision workload as this package calls it; the
// platform's *inference.Assembly satisfies it.
type DecisionSource interface {
	Decision(ctx context.Context, ref model.ModelRef, request inference.DecisionRequest) (inference.DecisionResponse, error)
}

// Identity implements Arbiter: the model, the wording, and the threshold, in
// the same shape the vocabulary's digest names its rules.
func (arbiter *DecisionArbiter) Identity() string {
	if arbiter == nil {
		return "none"
	}
	return fmt.Sprintf("%s/%s@%s,p=%g",
		arbiter.Model.ID.Provider, arbiter.Model.ID.Name,
		arbiterQuestionVersion, arbiter.threshold())
}

func (arbiter *DecisionArbiter) threshold() float64 {
	if arbiter == nil || arbiter.Threshold == 0 {
		return defaultArbiterThreshold
	}
	return arbiter.Threshold
}

func (arbiter *DecisionArbiter) batch() int {
	if arbiter == nil || arbiter.Batch <= 0 {
		return defaultArbiterBatch
	}
	return arbiter.Batch
}

// Adjudicate implements Arbiter. Pairs are answered in batches; a call that
// fails fails the whole answer, because a response is answered in question
// order and a partially-decoded one cannot be trusted to line up with the
// pairs it names.
func (arbiter *DecisionArbiter) Adjudicate(ctx context.Context, pairs []Pair) ([]Judgment, error) {
	if arbiter == nil || arbiter.Decisions == nil {
		return nil, errors.New("maintain: arbiter has no decision surface")
	}
	if ctx == nil {
		return nil, errors.New("maintain: arbiter requires a context")
	}
	if err := arbiter.Model.Validate(); err != nil {
		return nil, fmt.Errorf("maintain: arbiter model: %w", err)
	}
	judgments := make([]Judgment, len(pairs))
	batch := arbiter.batch()
	for start := 0; start < len(pairs); start += batch {
		end := min(start+batch, len(pairs))
		request, err := arbiterRequest(pairs[start:end])
		if err != nil {
			return nil, err
		}
		response, err := arbiter.Decisions.Decision(ctx, arbiter.Model, request)
		if err != nil {
			return nil, fmt.Errorf("maintain: adjudicate pairs %d..%d: %w", start, end-1, err)
		}
		if err := response.ValidateFor(request); err != nil {
			return nil, fmt.Errorf("maintain: adjudicate pairs %d..%d: %w", start, end-1, err)
		}
		for index, answer := range response.Answers {
			judgments[start+index] = arbiter.judgment(answer)
		}
	}
	return judgments, nil
}

// arbiterRequest asks one question per pair. The pair is stated in the
// question's own text, with the two turns' time expressions beside their
// statements when the extractor recorded one: they are evidence, not the
// answer -- the pass has already decided which statement is the later one, and
// a model reading "in 2019" beside "last month" agrees with that rather than
// re-deciding it.
func arbiterRequest(pairs []Pair) (inference.DecisionRequest, error) {
	questions := make([]inference.DecisionQuestion, len(pairs))
	for index, pair := range pairs {
		questions[index] = inference.DecisionQuestion{
			Name:         fmt.Sprintf("pair-%d", index),
			Kind:         inference.DecisionQuestionChoice,
			Instructions: arbiterPairQuestion(pair),
			Choices: []inference.DecisionChoice{
				{
					Value:       inference.NewDecisionStringChoice(string(VerdictReplaced)),
					Description: arbiterReplacedMeans,
				},
				{
					Value:       inference.NewDecisionStringChoice(string(VerdictBothCurrent)),
					Description: arbiterBothMeans,
				},
			},
		}
	}
	request := inference.DecisionRequest{
		Input:     inference.DecisionInput{Text: arbiterInput},
		Questions: questions,
	}
	if err := request.Validate(); err != nil {
		return inference.DecisionRequest{}, fmt.Errorf("maintain: arbiter request: %w", err)
	}
	return request, nil
}

// arbiterPairQuestion states one pair and asks the question about it.
func arbiterPairQuestion(pair Pair) string {
	var question strings.Builder
	question.WriteString("Older statement: ")
	question.WriteString(strings.TrimSpace(pair.Older.Text))
	writeTemporalDetail(&question, pair.Older.TemporalDetail)
	question.WriteString("\nNewer statement: ")
	question.WriteString(strings.TrimSpace(pair.Newer.Text))
	writeTemporalDetail(&question, pair.Newer.TemporalDetail)
	question.WriteString("\n")
	question.WriteString(arbiterInstructions)
	return question.String()
}

func writeTemporalDetail(question *strings.Builder, detail string) {
	if detail = strings.TrimSpace(detail); detail == "" {
		return
	}
	question.WriteString(" (stated as ")
	question.WriteString(detail)
	question.WriteString(")")
}

// judgment reads one answer. Only a string choice of "replaced" whose
// probability clears the threshold becomes a finding; everything else -- a
// refusal, an option the request did not offer, a probability below the
// threshold -- is undecided, which leaves the older fact current.
func (arbiter *DecisionArbiter) judgment(answer inference.DecisionAnswer) Judgment {
	if answer.Kind != inference.DecisionAnswerChoice || answer.Choice == nil ||
		answer.Choice.Kind != inference.DecisionChoiceValueString {
		return Judgment{Verdict: VerdictUndecided}
	}
	probability := 0.0
	for _, option := range answer.Probabilities {
		if option.Value == *answer.Choice {
			probability = option.Probability
		}
	}
	switch Verdict(answer.Choice.String) {
	case VerdictReplaced:
		if probability < arbiter.threshold() {
			return Judgment{Verdict: VerdictUndecided, Confidence: probability}
		}
		return Judgment{Verdict: VerdictReplaced, Confidence: probability}
	case VerdictBothCurrent:
		return Judgment{Verdict: VerdictBothCurrent, Confidence: probability}
	}
	return Judgment{Verdict: VerdictUndecided}
}
