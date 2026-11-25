package route

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
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

// generationRequest asks by model name only: the dispatch tests are about the loop, and
// a name-only request is the question a target with no declared line-up can answer.
// Provider pinning has its own tests below.
func generationRequest() inference.GenerateRequest {
	return inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Name: "m"}},
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
		Model:     model.ModelRef{ID: model.ModelID{Name: "m"}},
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

// A router publishes only the modes its targets serve.
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

// declaring builds a target whose catalog lists one model id.
func declaring(name string, gen inference.Generator, id model.ModelID) Target {
	return Target{
		Name:     name,
		Provider: inference.Provider{Generator: gen, Catalog: testCatalog{models: []model.ModelDescriptor{{ID: id}}}},
	}
}

// TestFailoverDoesNotCrossVendors is the provider half of "one model id": two vendors
// may serve a model with the same name, and a caller that named one of them must not be
// answered by the other — not even when the named one fails.
func TestFailoverDoesNotCrossVendors(t *testing.T) {
	mine := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnknownModel, inference.FieldModel, errors.New("down"))
	}}
	theirs := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		t.Error("a target from another vendor was tried")
		return inference.GenerateResponse{}, nil
	}}
	router, err := New([]Target{
		declaring("mine", mine, model.ModelID{Provider: "openai", Name: "shared"}),
		declaring("theirs", theirs, model.ModelID{Provider: "anthropic", Name: "shared"}),
	}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	request := generationRequest()
	request.Model = model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "shared"}}
	if _, err := router.Generate(context.Background(), request); err == nil {
		t.Fatal("Generate = nil error, want the named target's failure")
	}
	if mine.calls != 1 {
		t.Fatalf("the named target ran %d times, want once", mine.calls)
	}
	if theirs.calls != 0 {
		t.Fatalf("another vendor's target ran %d times, want never", theirs.calls)
	}
}

// TestANameOnlyRequestStillRoutes keeps the looser question working: a caller that
// names no provider is asking for any target that serves the model by name.
func TestANameOnlyRequestStillRoutes(t *testing.T) {
	for _, vendor := range []string{"openai", "anthropic"} {
		gen := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
			return inference.GenerateResponse{
				Message:      message.Message{Role: message.RoleAssistant, Content: message.NewTextContent("ok")},
				FinishReason: inference.FinishStop,
			}, nil
		}}
		router, err := New([]Target{declaring(vendor, gen, model.ModelID{Provider: vendor, Name: "shared"})}, Policy{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		request := generationRequest()
		request.Model = model.ModelRef{ID: model.ModelID{Name: "shared"}}
		if _, err := router.Generate(context.Background(), request); err != nil {
			t.Fatalf("%s: Generate: %v", vendor, err)
		}
	}
}

// TestADeclaredTargetModelPinsItsVendor is the same rule for a deployment that names
// the model on the target itself.
func TestADeclaredTargetModelPinsItsVendor(t *testing.T) {
	first := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnknownModel, inference.FieldModel, errors.New("down"))
	}}
	second := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		t.Error("a declared target with another vendor was tried")
		return inference.GenerateResponse{}, nil
	}}
	router, err := New([]Target{
		{Name: "a", Provider: inference.Provider{Generator: first}, Model: model.ModelID{Provider: "openai", Name: "shared"}},
		{Name: "b", Provider: inference.Provider{Generator: second}, Model: model.ModelID{Provider: "anthropic", Name: "shared"}},
	}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := generationRequest()
	request.Model = model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "shared"}}
	if _, err := router.Generate(context.Background(), request); err == nil {
		t.Fatal("Generate = nil error, want the named target's failure")
	}
	if second.calls != 0 {
		t.Fatalf("the other vendor's declared target ran %d times, want never", second.calls)
	}
}

// TestAnUndeclaredLineUpServesOnlyWhatItIsNamedFor is the second half of provider
// pinning: a target that publishes no line-up cannot prove it serves a model, so it is a
// candidate for a name-only request and for one that names it — not for every vendor.
func TestAnUndeclaredLineUpServesOnlyWhatItIsNamedFor(t *testing.T) {
	gen := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{
			Message:      message.Message{Role: message.RoleAssistant, Content: message.NewTextContent("ok")},
			FinishReason: inference.FinishStop,
		}, nil
	}}
	router, err := New([]Target{{Name: "openai", Provider: inference.Provider{Generator: gen}}}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Named for it: the target answers.
	request := generationRequest()
	request.Model = model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "m"}}
	if _, err := router.Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate for the named provider: %v", err)
	}

	// Pinned to another vendor: it is not a candidate, so nothing runs.
	before := gen.calls
	request.Model = model.ModelRef{ID: model.ModelID{Provider: "anthropic", Name: "m"}}
	if _, err := router.Generate(context.Background(), request); err == nil {
		t.Fatal("Generate = nil error, want no candidate")
	}
	if gen.calls != before {
		t.Fatalf("the undeclared target ran %d times for another vendor", gen.calls-before)
	}
}
