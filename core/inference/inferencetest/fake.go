package inferencetest

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
)

// DefaultFakeModel is GenerateFake's default model ref.
var DefaultFakeModel = inference.ModelRef{
	ID:      inference.ModelID{Provider: "fake", Name: "echo"},
	Profile: "default",
}

// GenerateFake is a canned Generate provider for tests: one provider,
// one profile, one model. Unary calls answer via Respond, streams play
// back Events, and every request reaching the compiler is captured for
// assertion. It drives the real Runtime resolve/compile/validate
// pipeline — nothing is mocked away, only the transport is canned.
type GenerateFake struct {
	// Model is the ref the runtime resolves. Defaults to
	// DefaultFakeModel.
	Model inference.ModelRef
	// Descriptor overrides the model's discovery metadata. The zero
	// value falls back to {ID: Model.ID}, so tests can declare limits
	// or lifecycle without rebuilding the provider.
	Descriptor inference.ModelDescriptor
	// Respond answers unary Generate calls. Default: a one-part "ok"
	// text message with inference.FinishCompleted.
	Respond func(inference.GenerateRequest) inference.GenerateResponse
	// Events plays back on GenerateStream. Default: a text delta "ok"
	// followed by inference.FinishCompleted.
	Events []inference.GenerateStreamEvent
	// StreamErr, when non-nil, makes GenerateStream's Next fail with
	// it once StreamErrAt events have been played back — a mid-stream
	// failure (connection reset, run interruption, …). StreamErrAt
	// defaults to len(Events): the stream fails after full playback,
	// before EOF.
	StreamErr   error
	StreamErrAt int

	// ExtensionDecoders are carried on the fake provider definition,
	// keyed by extension ID (see inference.ProviderDefinition).
	ExtensionDecoders map[string]inference.ExtensionDecoder

	mu       sync.Mutex
	requests []inference.GenerateRequest
}

// Assembly builds the fake's inference assembly.
func (f *GenerateFake) Assembly(t *testing.T) *inference.Assembly {
	t.Helper()
	model := f.Model
	if model.ID.Provider == "" {
		model = DefaultFakeModel
	}
	respond := f.Respond
	if respond == nil {
		respond = func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role:    message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{message.TextPart{Text: "ok"}}},
				},
				FinishReason: inference.FinishCompleted,
			}
		}
	}
	events := f.Events
	if events == nil {
		events = []inference.GenerateStreamEvent{
			{PartIndex: 0, Delta: inference.TextPartDelta{Text: "ok"}},
			{FinishReason: inference.FinishCompleted},
		}
	}

	compile := inference.GenerateCompiler[string](
		func(_ context.Context, _ inference.ModelRef, req inference.GenerateRequest, shape inference.GenerateExecutionShape) (inference.Compiled[string], error) {
			f.mu.Lock()
			f.requests = append(f.requests, req.Clone())
			f.mu.Unlock()
			return inference.Compiled[string]{
				Wire:   "wire",
				Report: NativeReport(inference.OperationGenerate, req.ActiveFieldsFor(shape)...),
			}, nil
		},
	)
	transport := inference.Transport[string, string](
		func(_ context.Context, wire string) (string, error) { return wire, nil },
	)
	decode := inference.Decoder[string, inference.GenerateResponse](
		func(_ context.Context, _ string) (inference.GenerateResponse, error) {
			return respond(f.lastRequest()), nil
		},
	)
	streamTransport := inference.Transport[string, inference.ProviderStream[inference.GenerateStreamEvent]](
		func(_ context.Context, _ string) (inference.ProviderStream[inference.GenerateStreamEvent], error) {
			errAt := f.StreamErrAt
			if errAt == 0 {
				errAt = len(events)
			}
			return &eventStream{events: events, err: f.StreamErr, errAt: errAt}, nil
		},
	)
	streamDecode := inference.GenerateStreamDecoder[inference.GenerateStreamEvent](
		func(_ context.Context, event inference.GenerateStreamEvent) (inference.GenerateStreamEvent, error) {
			return event, nil
		},
	)
	operations, err := inference.BindGenerateOperations(compile, transport, decode, streamTransport, streamDecode)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	descriptor := f.Descriptor
	if descriptor.ID.Provider == "" {
		descriptor = inference.ModelDescriptor{ID: model.ID}
	}
	definition := inference.ProviderDefinition{
		ID:                model.ID.Provider,
		ExtensionDecoders: f.ExtensionDecoders,
		Profiles: []inference.ProfileDefinition{{
			ID:         model.Profile,
			Operations: []inference.Operation{inference.OperationGenerate},
		}},
		Models: []inference.ModelImplementation{{
			Descriptor: descriptor,
			Openers: inference.Openers{
				Generate: func(_ context.Context, _ inference.ModelRef) (inference.GenerateOperations, error) {
					return operations, nil
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
// order. Route flows compile twice per attempt — Explain first,
// execution second — so a routed call shows up as two entries; see
// LastRequest for the executed one.
func (f *GenerateFake) Requests() []inference.GenerateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]inference.GenerateRequest(nil), f.requests...)
}

// LastRequest returns the most recently compiled request. For route
// flows the executed attempt is always the last compile (Explain
// calls precede it); for direct runtime calls it is the only one.
func (f *GenerateFake) LastRequest() inference.GenerateRequest {
	return f.lastRequest()
}

func (f *GenerateFake) lastRequest() inference.GenerateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return inference.GenerateRequest{}
	}
	return f.requests[len(f.requests)-1]
}

// StaticGenerateSelector returns a route.GenerateSelector that always
// selects ref on the primary tier — the smallest router wiring for
// tests that need the route path without a policy engine.
func StaticGenerateSelector(ref inference.ModelRef) route.GenerateSelector {
	return generateSelectorFunc(func(context.Context, inference.GenerateRequest) (route.Decision, error) {
		return route.Decision{
			Operation: inference.OperationGenerate,
			Tier:      "primary",
			Proposed:  ref,
			Selected:  ref,
		}, nil
	})
}

type generateSelectorFunc func(context.Context, inference.GenerateRequest) (route.Decision, error)

func (f generateSelectorFunc) SelectGenerate(ctx context.Context, req inference.GenerateRequest) (route.Decision, error) {
	return f(ctx, req)
}

// StaticEmbedSelector returns a route.EmbedSelector that always
// selects ref on the primary tier — the smallest router wiring for
// tests that need the embed route path without a policy engine.
func StaticEmbedSelector(ref inference.ModelRef) route.EmbedSelector {
	return embedSelectorFunc(func(context.Context, inference.EmbedRequest) (route.Decision, error) {
		return route.Decision{
			Operation: inference.OperationEmbed,
			Tier:      "primary",
			Proposed:  ref,
			Selected:  ref,
		}, nil
	})
}

type embedSelectorFunc func(context.Context, inference.EmbedRequest) (route.Decision, error)

func (f embedSelectorFunc) SelectEmbed(ctx context.Context, req inference.EmbedRequest) (route.Decision, error) {
	return f(ctx, req)
}

// eventStream is a ProviderStream over canned events.
type eventStream struct {
	events []inference.GenerateStreamEvent
	index  int
	err    error
	errAt  int
}

func (s *eventStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	if s.err != nil && s.index >= s.errAt {
		return inference.GenerateStreamEvent{}, s.err
	}
	if s.index >= len(s.events) {
		return inference.GenerateStreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *eventStream) Close() error { return nil }
