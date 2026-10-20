package inferencetest

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/resource"
)

// DefaultFakeDecisionModel is DecisionFake's default model ref.
var DefaultFakeDecisionModel = model.ModelRef{
	ID:      model.ModelID{Provider: "fake", Name: "decision"},
	Profile: "default",
}

// DecisionFake is a canned decision provider for tests: one provider, one
// profile, one model. Execute answers via Respond, and every request reaching
// the compiler is captured for assertion. It drives the real
// resolve/compile/validate pipeline.
type DecisionFake struct {
	// Model is the ref the runtime resolves. Defaults to
	// DefaultFakeDecisionModel.
	Model model.ModelRef
	// Descriptor overrides the model's discovery metadata. The zero value
	// falls back to {ID: Model.ID}.
	Descriptor model.ModelDescriptor
	// Respond answers Decision calls. Default: FakeDecisionAnswer for every
	// question.
	Respond func(inference.DecisionRequest) inference.DecisionResponse

	mu        sync.Mutex
	requests  []inference.DecisionRequest
	transport Counter
}

// Requests returns the captured compiler requests in order.
func (f *DecisionFake) Requests() []inference.DecisionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]inference.DecisionRequest(nil), f.requests...)
}

// TransportCalls returns how many times the provider transport ran.
func (f *DecisionFake) TransportCalls() int64 {
	return f.transport.Load()
}

// FakeDecisionAnswer builds one canned answer per question kind: a 0.75
// predicate probability, an even distribution over the question's options, or
// an even distribution over its levels. It is what DecisionFake answers with
// by default, and hosts can reuse it as a fixture.
func FakeDecisionAnswer(question inference.DecisionQuestion) inference.DecisionAnswer {
	switch question.Kind {
	case inference.DecisionQuestionChoice:
		share := 1.0 / float64(len(question.Choices))
		probabilities := make([]inference.DecisionOptionProbability, len(question.Choices))
		for index, choice := range question.Choices {
			probabilities[index] = inference.DecisionOptionProbability{
				Value:       choice.Value,
				Probability: share,
			}
		}
		chosen := question.Choices[0].Value
		confidence := share
		return inference.DecisionAnswer{
			Name:          question.Name,
			Kind:          inference.DecisionAnswerChoice,
			Choice:        &chosen,
			Confidence:    &confidence,
			Probabilities: probabilities,
		}
	case inference.DecisionQuestionScore:
		share := 1.0 / float64(len(question.Levels))
		levels := make([]inference.DecisionLevelProbability, len(question.Levels))
		for index, level := range question.Levels {
			levels[index] = inference.DecisionLevelProbability{
				Level:       index,
				Label:       level.Label,
				Probability: share,
			}
		}
		score := float64(len(question.Levels)-1) / 2
		confidence := share
		return inference.DecisionAnswer{
			Name:       question.Name,
			Kind:       inference.DecisionAnswerScore,
			Score:      &score,
			Levels:     levels,
			Confidence: &confidence,
		}
	default:
		probability := 0.75
		return inference.DecisionAnswer{
			Name:        question.Name,
			Kind:        inference.DecisionAnswerPredicate,
			Probability: &probability,
		}
	}
}

// FakeDecisionResponse answers every question in request with
// FakeDecisionAnswer.
func FakeDecisionResponse(request inference.DecisionRequest) inference.DecisionResponse {
	answers := make([]inference.DecisionAnswer, 0, len(request.Questions))
	for _, question := range request.Questions {
		answers = append(answers, FakeDecisionAnswer(question))
	}
	return inference.DecisionResponse{Answers: answers}
}

// Assembly builds the fake's inference assembly.
func (f *DecisionFake) Assembly(t *testing.T) *inference.Assembly {
	t.Helper()
	ref := f.Model
	if ref.ID.Provider == "" {
		ref = DefaultFakeDecisionModel
	}
	respond := f.Respond
	if respond == nil {
		respond = FakeDecisionResponse
	}
	compile := inference.Compiler[inference.DecisionRequest, string](
		func(
			_ context.Context,
			_ model.ModelRef,
			req inference.DecisionRequest,
		) (inference.Compiled[string], error) {
			f.mu.Lock()
			f.requests = append(f.requests, req.Clone())
			f.mu.Unlock()
			return inference.Compiled[string]{
				Wire:   "wire",
				Report: NativeReport(model.OperationDecision, req.ActiveFields()...),
			}, nil
		},
	)
	transport := inference.Transport[string, string](
		func(_ context.Context, wire string) (string, error) {
			f.transport.Inc()
			return wire, nil
		},
	)
	decode := inference.Decoder[string, inference.DecisionResponse](
		func(_ context.Context, _ string) (inference.DecisionResponse, error) {
			return respond(f.lastRequest()), nil
		},
	)
	driver, err := inference.BindDecision(compile, transport, decode)
	if err != nil {
		t.Fatalf("BindDecision: %v", err)
	}
	descriptor := f.Descriptor
	if descriptor.ID.Provider == "" {
		descriptor = model.ModelDescriptor{ID: ref.ID}
	}
	definition := inference.ProviderDefinition{
		ID: ref.ID.Provider,
		Profiles: []inference.ProfileDefinition{{
			ID:         ref.Profile,
			Operations: []model.Operation{model.OperationDecision},
		}},
		Models: []inference.ModelImplementation{{
			Descriptor: descriptor,
			Openers: inference.Openers{
				Decision: func(
					_ context.Context,
					_ model.ModelRef,
				) (inference.DecisionDriver, error) {
					return driver, nil
				},
			},
		}},
	}
	assembly, err := inference.Factory{}.New(context.Background(), resource.NewInput(nil,
		map[string]any{"provider." + ref.ID.Provider: definition}, nil, nil))
	if err != nil {
		t.Fatalf("build assembly: %v", err)
	}
	return assembly
}

func (f *DecisionFake) lastRequest() inference.DecisionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return inference.DecisionRequest{}
	}
	return f.requests[len(f.requests)-1]
}

// DecisionUnarySuite verifies the decision driver contract through the shared
// UnarySuite.
type DecisionUnarySuite struct {
	Model   model.ModelRef
	Request func() inference.DecisionRequest
	Driver  inference.DecisionDriver

	TransportCalls func() int64
	AssertResponse func(*testing.T, inference.DecisionResponse)
}

func RunDecisionUnary(t *testing.T, suite DecisionUnarySuite) {
	t.Helper()
	if suite.Driver == nil {
		t.Fatal("DecisionUnarySuite requires a driver")
	}
	assertResponse := suite.AssertResponse
	RunUnary(t, UnarySuite[inference.DecisionRequest, inference.DecisionResponse]{
		Operation: model.OperationDecision,
		Model:     suite.Model,
		Request:   suite.Request,
		Snapshot: func(request inference.DecisionRequest) any {
			return request.Clone()
		},
		Explain: suite.Driver.Explain,
		Execute: suite.Driver.Execute,
		Metadata: func(response inference.DecisionResponse) inference.Metadata {
			return response.Metadata
		},
		TransportCalls: suite.TransportCalls,
		AssertResponse: func(t *testing.T, response inference.DecisionResponse) {
			if response.Usage.Model != suite.Model {
				t.Fatalf(
					"Usage.Model = %+v, want %+v",
					response.Usage.Model,
					suite.Model,
				)
			}
			if response.Usage.LatencyMs < 0 {
				t.Fatalf(
					"Usage.LatencyMs = %d, want non-negative",
					response.Usage.LatencyMs,
				)
			}
			if assertResponse != nil {
				assertResponse(t, response)
			}
		},
	})
}
