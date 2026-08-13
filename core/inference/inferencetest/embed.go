package inferencetest

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

// DefaultFakeEmbedModel is EmbedFake's default model ref.
var DefaultFakeEmbedModel = inference.ModelRef{
	ID:      inference.ModelID{Provider: "fake", Name: "embed"},
	Profile: "default",
}

// EmbedFake is a canned Embed provider for tests: one provider, one
// profile, one model. Execute answers via Respond, and every request
// reaching the compiler is captured for assertion. It drives the real
// Runtime resolve/compile/validate pipeline.
type EmbedFake struct {
	// Model is the ref the runtime resolves. Defaults to
	// DefaultFakeEmbedModel.
	Model inference.ModelRef
	// Respond answers Embed calls. Default: one unit-length vector per
	// request item.
	Respond func(inference.EmbedRequest) inference.EmbedResponse

	mu       sync.Mutex
	requests []inference.EmbedRequest
}

// Assembly builds the fake's inference assembly.
func (f *EmbedFake) Assembly(t *testing.T) *inference.Assembly {
	t.Helper()
	model := f.Model
	if model.ID.Provider == "" {
		model = DefaultFakeEmbedModel
	}
	respond := f.Respond
	if respond == nil {
		respond = func(request inference.EmbedRequest) inference.EmbedResponse {
			embeddings := make([]inference.Embedding, len(request.Items))
			for index := range embeddings {
				embeddings[index] = inference.Embedding{Vector: []float32{1}}
			}
			return inference.EmbedResponse{Embeddings: embeddings}
		}
	}

	compile := inference.Compiler[inference.EmbedRequest, string](
		func(_ context.Context, _ inference.ModelRef, req inference.EmbedRequest) (inference.Compiled[string], error) {
			f.mu.Lock()
			f.requests = append(f.requests, req.Clone())
			f.mu.Unlock()
			return inference.Compiled[string]{
				Wire:   "wire",
				Report: NativeReport(inference.OperationEmbed, req.ActiveFields()...),
			}, nil
		},
	)
	transport := inference.Transport[string, string](
		func(_ context.Context, wire string) (string, error) { return wire, nil },
	)
	decode := inference.Decoder[string, inference.EmbedResponse](
		func(_ context.Context, _ string) (inference.EmbedResponse, error) {
			return respond(f.lastRequest()), nil
		},
	)
	driver, err := inference.BindEmbed(compile, transport, decode)
	if err != nil {
		t.Fatalf("BindEmbed: %v", err)
	}

	definition := inference.ProviderDefinition{
		ID: model.ID.Provider,
		Profiles: []inference.ProfileDefinition{{
			ID:         model.Profile,
			Operations: []inference.Operation{inference.OperationEmbed},
		}},
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{ID: model.ID},
			Openers: inference.Openers{
				Embed: func(_ context.Context, _ inference.ModelRef) (inference.EmbedDriver, error) {
					return driver, nil
				},
			},
		}},
	}
	value, err := inference.Factory{}.New(context.Background(), resource.Input{
		Deps: map[string]any{"provider." + model.ID.Provider: definition},
	})
	if err != nil {
		t.Fatalf("build assembly: %v", err)
	}
	return value.(*inference.Assembly)
}

// Requests returns the cloned requests that reached the compiler, in
// order.
func (f *EmbedFake) Requests() []inference.EmbedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]inference.EmbedRequest(nil), f.requests...)
}

// LastRequest returns the most recently compiled request.
func (f *EmbedFake) LastRequest() inference.EmbedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return inference.EmbedRequest{}
	}
	return f.requests[len(f.requests)-1]
}

func (f *EmbedFake) lastRequest() inference.EmbedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return inference.EmbedRequest{}
	}
	return f.requests[len(f.requests)-1]
}

// EmbedUnarySuite verifies the shared Runtime contracts for Embed
// drivers.
type EmbedUnarySuite struct {
	Model   inference.ModelRef
	Request func() inference.EmbedRequest
	Driver  inference.EmbedDriver

	TransportCalls func() int64
	AssertResponse func(*testing.T, inference.EmbedResponse)
}

// RunEmbedUnary runs the Embed-specific unary conformance suite.
func RunEmbedUnary(t *testing.T, suite EmbedUnarySuite) {
	t.Helper()
	if suite.Driver == nil {
		t.Fatal("EmbedUnarySuite requires a driver")
	}
	RunUnary(t, UnarySuite[inference.EmbedRequest, inference.EmbedResponse]{
		Operation: inference.OperationEmbed,
		Model:     suite.Model,
		Request:   suite.Request,
		Snapshot: func(request inference.EmbedRequest) any {
			return request.Clone()
		},
		Explain:        suite.Driver.Explain,
		Execute:        suite.Driver.Execute,
		Metadata:       func(response inference.EmbedResponse) inference.Metadata { return response.Metadata },
		TransportCalls: suite.TransportCalls,
		AssertResponse: suite.AssertResponse,
	})
}
