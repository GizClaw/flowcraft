package maintain

import (
	"context"
	"strings"
	"testing"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

func arbiterPair(older, newer string) Pair {
	return Pair{
		Conversation: "conv-1",
		Older:        factview.Fact{ID: "old-" + older, Text: older},
		Newer:        factview.Fact{ID: "new-" + newer, Text: newer},
	}
}

// choiceAnswer builds the answer the arbiter reads: a string choice of value
// with the probability the model put on it.
func choiceAnswer(name, value string, probability float64) inference.DecisionAnswer {
	chosen := inference.NewDecisionStringChoice(value)
	return inference.DecisionAnswer{
		Name: name, Kind: inference.DecisionAnswerChoice,
		Choice:     &chosen,
		Confidence: &probability,
		Probabilities: []inference.DecisionOptionProbability{
			{Value: chosen, Probability: probability},
			{
				Value:       inference.NewDecisionStringChoice(otherVerdict(value)),
				Probability: 1 - probability,
			},
		},
	}
}

func otherVerdict(value string) string {
	if value == string(VerdictReplaced) {
		return string(VerdictBothCurrent)
	}
	return string(VerdictReplaced)
}

// TestDecisionArbiterAsksOneQuestionPerPair pins the request the pass pays
// for: one call carries every pair in the batch, one choice question per pair
// over the same two options, and each question states its own pair -- the
// surface answers a question from that question's text, so a batch that put the
// pairs in a shared evidence blob and asked one wording thirteen times got one
// answer back, repeated.
func TestDecisionArbiterAsksOneQuestionPerPair(t *testing.T) {
	fake := &inferencetest.DecisionFake{Respond: func(request inference.DecisionRequest) inference.DecisionResponse {
		answers := make([]inference.DecisionAnswer, len(request.Questions))
		for index, question := range request.Questions {
			answers[index] = choiceAnswer(question.Name, string(VerdictReplaced), 0.9)
		}
		return inference.DecisionResponse{Answers: answers}
	}}
	arbiter := &DecisionArbiter{
		Decisions: fake.Assembly(t),
		Model:     inferencetest.DefaultFakeDecisionModel,
	}
	pairs := []Pair{
		arbiterPair("郑凯住在柏林。", "郑凯上个月搬到了上海。"),
		arbiterPair("Caroline works at Acme.", "Caroline works at Globex."),
	}
	judgments, err := arbiter.Adjudicate(context.Background(), pairs)
	if err != nil {
		t.Fatal(err)
	}
	if len(judgments) != 2 {
		t.Fatalf("judgments = %#v", judgments)
	}
	for index, judgment := range judgments {
		if judgment.Verdict != VerdictReplaced || judgment.Confidence != 0.9 {
			t.Fatalf("judgment %d = %+v", index, judgment)
		}
	}

	requests := fake.Requests()
	if len(requests) != 1 {
		t.Fatalf("calls = %d, want 1", len(requests))
	}
	request := requests[0]
	if len(request.Questions) != 2 {
		t.Fatalf("questions = %d, want 2", len(request.Questions))
	}
	for index, question := range request.Questions {
		if question.Name != "pair-"+string(rune('0'+index)) {
			t.Errorf("question %d is named %q", index, question.Name)
		}
		if question.Kind != inference.DecisionQuestionChoice {
			t.Errorf("question %d is a %s, want a choice", index, question.Kind)
		}
		pair := pairs[index]
		for _, statement := range []string{pair.Older.Text, pair.Newer.Text} {
			if !strings.Contains(question.Instructions, statement) {
				t.Errorf("question %d does not state %q: %q", index, statement, question.Instructions)
			}
		}
		if !strings.Contains(question.Instructions, arbiterInstructions) {
			t.Errorf("question %d does not ask the rule question: %q", index, question.Instructions)
		}
		if len(question.Choices) != 2 {
			t.Fatalf("question %d offers %d options", index, len(question.Choices))
		}
		offered := map[string]bool{}
		for _, choice := range question.Choices {
			offered[choice.Value.String] = true
		}
		if !offered[string(VerdictReplaced)] || !offered[string(VerdictBothCurrent)] {
			t.Errorf("question %d offers %#v", index, offered)
		}
	}
	// The questions differ, because the pairs do: thirteen identical questions
	// are what the batched-evidence shape collapsed into.
	if request.Questions[0].Instructions == request.Questions[1].Instructions {
		t.Error("two different pairs were asked the same question")
	}
	if request.Input.Text != arbiterInput {
		t.Errorf("input = %q, want the batch framing", request.Input.Text)
	}
}

// TestArbiterQuestionCarriesTheStatedTimes pins the evidence beside each
// statement: the extractor's own time expression is what tells a model that
// "上个月" in one statement and "2018" in another are not the same instant. It
// is evidence, not an instruction, so a pair without one reads the same either
// way.
func TestArbiterQuestionCarriesTheStatedTimes(t *testing.T) {
	older := factview.Fact{ID: "old", Text: "郑凯住在柏林。", TemporalDetail: "2018 年"}
	newer := factview.Fact{ID: "new", Text: "郑凯上个月搬到了上海。", TemporalDetail: "上个月"}
	question := arbiterPairQuestion(Pair{Older: older, Newer: newer})
	for _, want := range []string{"郑凯住在柏林。", "2018 年", "郑凯上个月搬到了上海。", "上个月"} {
		if !strings.Contains(question, want) {
			t.Errorf("question does not carry %q: %q", want, question)
		}
	}
	bare := arbiterPairQuestion(Pair{
		Older: factview.Fact{ID: "old", Text: "a"}, Newer: factview.Fact{ID: "new", Text: "b"},
	})
	if strings.Contains(bare, "stated as") {
		t.Errorf("a pair without stated times renders a time clause: %q", bare)
	}
}

// TestDecisionArbiterThresholdKeepsBorderlinePairsCurrent pins the asymmetry
// the threshold encodes: a finding penalizes a fact the pass cannot tell is
// stale, so a pair has to be answered clearly to become one. A refusal and a
// low probability both leave the older fact current, and neither is an error.
func TestDecisionArbiterThresholdKeepsBorderlinePairsCurrent(t *testing.T) {
	answers := map[string]inference.DecisionAnswer{
		"pair-0": choiceAnswer("pair-0", string(VerdictReplaced), 0.6),
		"pair-1": {Name: "pair-1", Kind: inference.DecisionAnswerRefusal},
		"pair-2": choiceAnswer("pair-2", string(VerdictBothCurrent), 0.95),
		"pair-3": choiceAnswer("pair-3", string(VerdictReplaced), 0.95),
	}
	fake := &inferencetest.DecisionFake{Respond: func(request inference.DecisionRequest) inference.DecisionResponse {
		response := inference.DecisionResponse{}
		for _, question := range request.Questions {
			response.Answers = append(response.Answers, answers[question.Name])
		}
		return response
	}}
	arbiter := &DecisionArbiter{Decisions: fake.Assembly(t), Model: inferencetest.DefaultFakeDecisionModel}
	pairs := []Pair{
		arbiterPair("a1", "a2"), arbiterPair("b1", "b2"),
		arbiterPair("c1", "c2"), arbiterPair("d1", "d2"),
	}
	judgments, err := arbiter.Adjudicate(context.Background(), pairs)
	if err != nil {
		t.Fatal(err)
	}
	want := []Verdict{VerdictUndecided, VerdictUndecided, VerdictBothCurrent, VerdictReplaced}
	for index, verdict := range want {
		if judgments[index].Verdict != verdict {
			t.Errorf("judgment %d = %+v, want %s", index, judgments[index], verdict)
		}
	}
	if judgments[0].Confidence != 0.6 {
		t.Errorf("borderline judgment dropped its probability: %+v", judgments[0])
	}
}

// TestDecisionArbiterBatches pins the per-call bound: the OpenAI wire takes
// 200 questions, and a pass whose budget is larger splits the pairs rather
// than sending a request the provider would reject.
func TestDecisionArbiterBatches(t *testing.T) {
	fake := &inferencetest.DecisionFake{}
	arbiter := &DecisionArbiter{
		// One call at a time, so the requests come back in the order the pairs
		// were proposed: which call finishes first is the schedule's business,
		// and that is what the concurrency tests pin instead.
		Decisions: fake.Assembly(t), Model: inferencetest.DefaultFakeDecisionModel,
		Batch: 2, Concurrency: 1,
	}
	pairs := []Pair{
		arbiterPair("a1", "a2"), arbiterPair("b1", "b2"), arbiterPair("c1", "c2"),
	}
	judgments, err := arbiter.Adjudicate(context.Background(), pairs)
	if err != nil {
		t.Fatal(err)
	}
	if len(judgments) != 3 {
		t.Fatalf("judgments = %#v", judgments)
	}
	requests := fake.Requests()
	if len(requests) != 2 {
		t.Fatalf("calls = %d, want 2", len(requests))
	}
	if len(requests[0].Questions) != 2 || len(requests[1].Questions) != 1 {
		t.Fatalf("batch sizes = %d, %d", len(requests[0].Questions), len(requests[1].Questions))
	}
}

// TestDecisionArbiterRequiresASurface pins the wiring failure: an arbiter
// configured without a decision surface or without a model is an error at the
// call rather than a pass that silently decides nothing.
func TestDecisionArbiterRequiresASurface(t *testing.T) {
	ctx := context.Background()
	pairs := []Pair{arbiterPair("a", "b")}
	if _, err := (&DecisionArbiter{}).Adjudicate(ctx, pairs); err == nil {
		t.Error("an arbiter without a decision surface answered")
	}
	fake := &inferencetest.DecisionFake{}
	if _, err := (&DecisionArbiter{Decisions: fake.Assembly(t)}).Adjudicate(ctx, pairs); err == nil {
		t.Error("an arbiter without a model answered")
	}
}

// TestArbiterIdentityStampsTheOverlay pins what makes an overlay comparable: a
// pass with an arbiter stamps the model, the wording, and the threshold into
// ProducerVersion, and a pass without one stamps exactly what it stamped
// before -- so adding an arbiter re-judges the scope, and leaving it out keeps
// the overlay a deployment already has.
func TestArbiterIdentityStampsTheOverlay(t *testing.T) {
	plain := Config{}.ProducerVersion()
	if strings.Contains(plain, "arbiter=") {
		t.Fatalf("a pass without an arbiter stamps one: %q", plain)
	}
	arbiter := &DecisionArbiter{
		Model: model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-6-luna"}},
	}
	if got, want := arbiter.Identity(), "openai/gpt-6-luna@supersede-v1,p=0.75"; got != want {
		t.Fatalf("identity = %q, want %q", got, want)
	}
	stamped := Config{Arbiter: arbiter}.ProducerVersion()
	if !strings.Contains(stamped, ",arbiter=openai/gpt-6-luna@supersede-v1,p=0.75") {
		t.Fatalf("stamp = %q", stamped)
	}
	// The threshold is part of the judgment, so raising it re-judges the
	// scope rather than serving what the lower threshold accepted.
	stricter := &DecisionArbiter{
		Model:     arbiter.Model,
		Threshold: 0.9,
	}
	if stricter.Identity() == arbiter.Identity() {
		t.Fatal("a threshold change is not in the identity")
	}
	// A typed nil in the interface is still an arbiter; it must not be
	// mistaken for a configured one.
	var nilArbiter *DecisionArbiter
	if got := nilArbiter.Identity(); got != "none" {
		t.Fatalf("nil arbiter identity = %q", got)
	}
	if got := (Config{Arbiter: nilArbiter}).ProducerVersion(); strings.Contains(got, "arbiter=none") {
		t.Fatalf("a typed nil arbiter stamped the overlay: %q", got)
	}
}
