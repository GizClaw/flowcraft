package route

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

type fakeGenerator struct {
	calls int
	reply func(call int) (inference.GenerateResponse, error)
}

func (f *fakeGenerator) Generate(_ context.Context, _ inference.GenerateRequest) (inference.GenerateResponse, error) {
	f.calls++
	return f.reply(f.calls)
}

func (f *fakeGenerator) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	return nil, errors.New("streaming not used")
}

type fakeDecider struct {
	calls int
	seen  []int // question counts per call
	reply func(call int) (inference.DecisionResponse, error)
}

func (f *fakeDecider) Decide(_ context.Context, request inference.DecisionRequest) (inference.DecisionResponse, error) {
	f.calls++
	f.seen = append(f.seen, len(request.Questions))
	return f.reply(f.calls)
}

func generateTarget(name string, gen inference.Generator) Target {
	return Target{Name: name, Provider: inference.Provider{Generator: gen}}
}

func generationRequest() inference.GenerateRequest {
	return inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: "p", Name: "m"}},
		Input: inference.GenerateInput{Content: message.NewTextContent("hi")},
	}
}

func decisionRequest(questions int) inference.DecisionRequest {
	input := make([]inference.Question, 0, questions)
	for i := 0; i < questions; i++ {
		input = append(input, inference.Question{
			Name:         "q" + string(rune('a'+i)),
			Kind:         inference.QuestionPredicate,
			Instructions: "judge it",
		})
	}
	return inference.DecisionRequest{
		Model:     model.ModelRef{ID: model.ModelID{Provider: "p", Name: "m"}},
		Input:     message.NewTextContent("material"),
		Questions: input,
	}
}

// A target that cannot serve the request is skipped; the next one answers, and
// the trace says why.
func TestDispatchSkipsTargetThatCannotServe(t *testing.T) {
	first := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnknownModel, inference.FieldModel, errors.New("not declared"))
	}}
	second := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{
			Message:      message.Message{Role: message.RoleAssistant, Content: message.NewTextContent("ok")},
			FinishReason: inference.FinishStop,
		}, nil
	}}
	router, err := New([]Target{generateTarget("a", first), generateTarget("b", second)}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	trace := &Trace{}
	response, err := router.Generate(WithTrace(context.Background(), trace), generationRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Message.Content.Text() != "ok" {
		t.Errorf("response came from the wrong target: %q", response.Message.Content.Text())
	}
	attempts := trace.Attempts()
	if len(attempts) != 2 || attempts[0].Target != "a" || attempts[1].Target != "b" {
		t.Fatalf("trace = %+v", attempts)
	}
	if attempts[0].Kind != inference.UnknownModel {
		t.Errorf("first attempt kind = %q, want UnknownModel", attempts[0].Kind)
	}
	if attempts[1].Kind != "" {
		t.Errorf("successful attempt recorded kind %q", attempts[1].Kind)
	}
}

// A request-level rejection stops immediately: trying another target would
// produce the same rejection.
func TestDispatchStopsOnInvalidRequest(t *testing.T) {
	first := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{}, inference.NewError(
			inference.InvalidRequest, inference.FieldInput, errors.New("bad input"))
	}}
	second := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		t.Error("second target must not be tried")
		return inference.GenerateResponse{}, nil
	}}
	router, err := New([]Target{generateTarget("a", first), generateTarget("b", second)}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := router.Generate(context.Background(), generationRequest()); err == nil {
		t.Fatal("Generate = nil error, want the request rejection")
	}
	if second.calls != 0 {
		t.Errorf("second target was tried %d times", second.calls)
	}
}

// A transient provider failure is retried on the same target before moving on.
func TestDispatchRetriesSameTarget(t *testing.T) {
	flaky := &fakeGenerator{reply: func(call int) (inference.GenerateResponse, error) {
		if call < 2 {
			return inference.GenerateResponse{}, inference.NewError(
				inference.ProviderFailure, "", errors.New("503"))
		}
		return inference.GenerateResponse{
			Message:      message.Message{Role: message.RoleAssistant, Content: message.NewTextContent("ok")},
			FinishReason: inference.FinishStop,
		}, nil
	}}
	router, err := New([]Target{generateTarget("a", flaky)}, Policy{Retry: RetryPolicy{AttemptsPerTarget: 2}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := router.Generate(context.Background(), generationRequest()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if flaky.calls != 2 {
		t.Errorf("calls = %d, want 2", flaky.calls)
	}
}

// One judgment call is answered by one model: failover re-asks every question
// rather than mixing answers from two models.
func TestDecisionFailoverReasksTheWholeCall(t *testing.T) {
	failing := &fakeDecider{reply: func(int) (inference.DecisionResponse, error) {
		return inference.DecisionResponse{}, inference.NewError(
			inference.ProviderResponseUnusable, inference.FieldAnswers, errors.New("garbled"))
	}}
	good := &fakeDecider{reply: func(int) (inference.DecisionResponse, error) {
		return inference.DecisionResponse{}, nil
	}}
	router, err := New([]Target{
		{Name: "weak", Provider: inference.Provider{Decider: failing}},
		{Name: "strong", Provider: inference.Provider{Decider: good}},
	}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := router.Decide(context.Background(), decisionRequest(3)); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if failing.calls != 1 || good.calls != 1 {
		t.Fatalf("calls: failing=%d good=%d, want one each", failing.calls, good.calls)
	}
	if len(good.seen) != 1 || good.seen[0] != 3 {
		t.Errorf("fallback target saw %v questions, want the whole call (3)", good.seen)
	}
}

// A router publishes only the seams its targets serve.
func TestProviderPublishesOnlyServedSeams(t *testing.T) {
	decider := &fakeDecider{reply: func(int) (inference.DecisionResponse, error) {
		return inference.DecisionResponse{}, nil
	}}
	router, err := New([]Target{{Name: "judge", Provider: inference.Provider{Decider: decider}}}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := router.Provider()
	if provider.Generator != nil {
		t.Error("a router with no generator published a Generator")
	}
	if provider.Decider == nil {
		t.Error("a router with a decider published no Decider")
	}
}
