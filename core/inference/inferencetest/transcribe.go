package inferencetest

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/resource"
)

// DefaultFakeTranscribeModel is TranscriptionFake's default model ref.
var DefaultFakeTranscribeModel = inference.ModelRef{
	ID:      inference.ModelID{Provider: "fake", Name: "transcribe"},
	Profile: "default",
}

// TranscriptionFake is a canned unary transcription provider for tests: one
// provider, one profile, one model. Execute answers via Respond, and every
// request reaching the compiler is captured for assertion. It drives the
// real resolve/compile/validate pipeline.
type TranscriptionFake struct {
	// Model is the ref the runtime resolves. Defaults to
	// DefaultFakeTranscribeModel.
	Model inference.ModelRef
	// Descriptor overrides the model's discovery metadata. The zero
	// value falls back to {ID: Model.ID}.
	Descriptor inference.ModelDescriptor
	// Respond answers Transcribe calls. Default: "ok" with one segment.
	Respond func(inference.TranscriptionRequest) inference.TranscriptionResponse
	// SessionEvents plays back on TranscribeSession. Default: one final
	// "ok" event.
	SessionEvents []inference.TranscriptionSessionEvent

	mu              sync.Mutex
	requests        []inference.TranscriptionRequest
	sessionRequests []inference.TranscriptionSessionRequest
}

// Requests returns the captured compiler requests in order.
func (f *TranscriptionFake) Requests() []inference.TranscriptionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]inference.TranscriptionRequest(nil), f.requests...)
}

// SessionRequests returns the captured session compiler requests in order.
func (f *TranscriptionFake) SessionRequests() []inference.TranscriptionSessionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]inference.TranscriptionSessionRequest(nil), f.sessionRequests...)
}

// Assembly builds the fake's inference assembly.
func (f *TranscriptionFake) Assembly(t *testing.T) *inference.Assembly {
	t.Helper()
	model := f.Model
	if model.ID.Provider == "" {
		model = DefaultFakeTranscribeModel
	}
	respond := f.Respond
	if respond == nil {
		respond = func(inference.TranscriptionRequest) inference.TranscriptionResponse {
			return inference.TranscriptionResponse{
				Text: "ok",
				Segments: []inference.TranscriptionSegment{{
					Text: "ok",
				}},
			}
		}
	}

	compile := inference.Compiler[inference.TranscriptionRequest, string](
		func(_ context.Context, _ inference.ModelRef, req inference.TranscriptionRequest) (inference.Compiled[string], error) {
			f.mu.Lock()
			f.requests = append(f.requests, req.Clone())
			f.mu.Unlock()
			return inference.Compiled[string]{
				Wire:   "wire",
				Report: NativeReport(inference.OperationTranscription, req.ActiveFields()...),
			}, nil
		},
	)
	transport := inference.Transport[string, string](
		func(_ context.Context, wire string) (string, error) { return wire, nil },
	)
	decode := inference.Decoder[string, inference.TranscriptionResponse](
		func(_ context.Context, _ string) (inference.TranscriptionResponse, error) {
			return respond(f.lastRequest()), nil
		},
	)
	sessionCompile := inference.Compiler[inference.TranscriptionSessionRequest, string](
		func(_ context.Context, _ inference.ModelRef, req inference.TranscriptionSessionRequest) (inference.Compiled[string], error) {
			f.mu.Lock()
			f.sessionRequests = append(f.sessionRequests, req.Clone())
			f.mu.Unlock()
			return inference.Compiled[string]{
				Wire:   "wire",
				Report: NativeReport(inference.OperationTranscription, req.ActiveFields()...),
			}, nil
		},
	)
	events := f.SessionEvents
	if events == nil {
		events = []inference.TranscriptionSessionEvent{{
			Text:  "ok",
			Final: true,
		}}
	}
	sessionTransport := inference.TranscriptionSessionTransport[string, inference.TranscriptionSessionEvent](
		func(_ context.Context, _ string) (inference.ProviderSession[inference.TranscriptionSessionEvent], error) {
			return &fakeProviderSession{events: events}, nil
		},
	)
	sessionDecode := inference.TranscriptionSessionDecoder[inference.TranscriptionSessionEvent](
		func(_ context.Context, event inference.TranscriptionSessionEvent) (inference.TranscriptionSessionEvent, error) {
			return event, nil
		},
	)
	operations, err := inference.BindTranscribeOperations(
		compile,
		transport,
		decode,
		sessionCompile,
		sessionTransport,
		sessionDecode,
	)
	if err != nil {
		t.Fatalf("BindTranscribeOperations: %v", err)
	}
	descriptor := f.Descriptor
	if descriptor.ID.Provider == "" {
		descriptor = inference.ModelDescriptor{ID: model.ID}
	}

	definition := inference.ProviderDefinition{
		ID: model.ID.Provider,
		Profiles: []inference.ProfileDefinition{{
			ID:         model.Profile,
			Operations: []inference.Operation{inference.OperationTranscription},
		}},
		Models: []inference.ModelImplementation{{
			Descriptor: descriptor,
			Openers: inference.Openers{
				Transcribe: func(
					_ context.Context,
					_ inference.ModelRef,
				) (inference.TranscribeOperations, error) {
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

func (f *TranscriptionFake) lastRequest() inference.TranscriptionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return inference.TranscriptionRequest{}
	}
	return f.requests[len(f.requests)-1]
}

// fakeProviderSession is the canned provider session behind
// TranscriptionFake's session driver: it records sends and plays back the
// configured canonical events (the decoder is the identity function).
type fakeProviderSession struct {
	mu     sync.Mutex
	events []inference.TranscriptionSessionEvent
	sends  int
}

func (s *fakeProviderSession) Send(
	context.Context,
	media.AudioChunk,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends++
	return nil
}

func (s *fakeProviderSession) Next(
	context.Context,
) (inference.TranscriptionSessionEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return inference.TranscriptionSessionEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (s *fakeProviderSession) Interrupt() error { return nil }
func (s *fakeProviderSession) Close() error     { return nil }

// TranscriptionUnarySuite verifies the unary transcription driver contract
// through the shared UnarySuite.
type TranscriptionUnarySuite struct {
	Model   inference.ModelRef
	Request func() inference.TranscriptionRequest
	Driver  inference.TranscriptionDriver

	TransportCalls func() int64
	AssertResponse func(*testing.T, inference.TranscriptionResponse)
}

func RunTranscribeUnary(t *testing.T, suite TranscriptionUnarySuite) {
	t.Helper()
	if suite.Driver == nil {
		t.Fatal("TranscriptionUnarySuite requires a driver")
	}
	assertResponse := suite.AssertResponse
	RunUnary(t, UnarySuite[inference.TranscriptionRequest, inference.TranscriptionResponse]{
		Operation: inference.OperationTranscription,
		Model:     suite.Model,
		Request:   suite.Request,
		Snapshot: func(request inference.TranscriptionRequest) any {
			return request.Clone()
		},
		Explain: suite.Driver.Explain,
		Execute: suite.Driver.Execute,
		Metadata: func(response inference.TranscriptionResponse) inference.Metadata {
			return response.Metadata
		},
		TransportCalls: suite.TransportCalls,
		AssertResponse: func(t *testing.T, response inference.TranscriptionResponse) {
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

// TranscriptionSessionSuite verifies the duplex transcription session
// contract: Explain without provider I/O, Open with exactly one transport
// call, Send/Next over the scripted provider session, Result with stamped
// metadata, and Close.
type TranscriptionSessionSuite struct {
	Model   inference.ModelRef
	Request func() inference.TranscriptionSessionRequest
	Driver  inference.TranscriptionSessionDriver

	// Feed sends audio chunks into the opened session before draining.
	// Default: one valid PCM chunk.
	Feed           func(context.Context, inference.TranscriptionSession) error
	TransportCalls func() int64
	AssertEvent    func(*testing.T, inference.TranscriptionSessionEvent)
	AssertResult   func(*testing.T, inference.TranscriptionResponse)
	AssertClose    func(*testing.T, error)
}

func RunTranscribeSession(t *testing.T, suite TranscriptionSessionSuite) {
	t.Helper()
	if suite.Request == nil || suite.Driver == nil ||
		suite.TransportCalls == nil {
		t.Fatal(
			"TranscriptionSessionSuite requires request, driver, and transport probe",
		)
	}
	if err := suite.Model.Validate(); err != nil {
		t.Fatalf("Model: %v", err)
	}

	request := suite.Request()
	expected := request.Clone()
	before := suite.TransportCalls()
	explanation, err := suite.Driver.Explain(
		context.Background(),
		suite.Model,
		request,
	)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if suite.TransportCalls() != before ||
		explanation.Operation != inference.OperationTranscription ||
		len(explanation.Decisions) == 0 {
		t.Fatalf("Explain performed I/O or lost decisions: %+v", explanation)
	}
	assertUnchanged(t, expected, request.Clone())

	session, err := suite.Driver.Open(
		context.Background(),
		suite.Model,
		request,
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if suite.TransportCalls() != before+1 {
		t.Fatalf(
			"Open transport calls = %d, want %d",
			suite.TransportCalls(),
			before+1,
		)
	}
	feed := suite.Feed
	if feed == nil {
		feed = func(ctx context.Context, session inference.TranscriptionSession) error {
			return session.Send(ctx, media.AudioChunk{Data: []byte{0, 0}})
		}
	}
	if err := feed(context.Background(), session); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	for {
		event, err := session.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if suite.AssertEvent != nil {
			suite.AssertEvent(t, event)
		}
	}
	response, err := session.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if response.Metadata.Model != suite.Model.ID ||
		response.Metadata.Operation != inference.OperationTranscription ||
		len(response.Metadata.Decisions) == 0 {
		t.Fatalf("Result metadata = %+v", response.Metadata)
	}
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
	if suite.AssertResult != nil {
		suite.AssertResult(t, response)
	}
	closeErr := session.Close()
	if suite.AssertClose != nil {
		suite.AssertClose(t, closeErr)
	} else if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	assertUnchanged(t, expected, request.Clone())
}
