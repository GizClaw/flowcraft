package route

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/resource"
)

type routeWire struct{}
type routeRaw struct{ Text string }

func routeCompiler() inference.GenerateCompiler[routeWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[routeWire], error) {
		active := request.ActiveFieldsFor(shape)
		decisions := make([]inference.Decision, len(active))
		for index, field := range active {
			decisions[index] = inference.Decision{Field: field, Disposition: inference.Native}
		}
		return inference.Compiled[routeWire]{
			Wire: routeWire{},
			Report: inference.CompileReport{
				Operation: inference.OperationGenerate,
				Decisions: decisions,
			},
		}, nil
	}
}

func routeTransport(fail bool) inference.Transport[routeWire, routeRaw] {
	return func(context.Context, routeWire) (routeRaw, error) {
		if fail {
			return routeRaw{}, errdefs.NotAvailablef("upstream unavailable")
		}
		return routeRaw{Text: "ok"}, nil
	}
}

func routeDecode() inference.Decoder[routeRaw, inference.GenerateResponse] {
	return func(_ context.Context, raw routeRaw) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{
			Message: message.Message{
				Role: message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: raw.Text},
				}},
			},
			FinishReason: inference.FinishCompleted,
			Usage: inference.Usage{
				InputTokens:  3,
				OutputTokens: 4,
				TotalTokens:  7,
			},
		}, nil
	}
}

func imageRouteDecode() inference.Decoder[routeRaw, inference.GenerateResponse] {
	return func(_ context.Context, _ routeRaw) (inference.GenerateResponse, error) {
		source, err := media.NewImageURL("https://example.com/out.png", "image/png")
		if err != nil {
			return inference.GenerateResponse{}, err
		}
		return inference.GenerateResponse{
			Message: message.Message{
				Role: message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{
					message.ImagePart{Source: source},
				}},
			},
			FinishReason: inference.FinishCompleted,
			Usage: inference.Usage{
				InputTokens:  3,
				OutputTokens: 4,
				TotalTokens:  7,
			},
		}, nil
	}
}

func providerDefinition(
	t *testing.T,
	id string,
	fail bool,
) inference.ProviderDefinition {
	return providerDefinitionWithOutputs(t, id, fail, nil, routeDecode())
}

func providerDefinitionWithOutputs(
	t *testing.T,
	id string,
	fail bool,
	outputs []message.PartKind,
	decode inference.Decoder[routeRaw, inference.GenerateResponse],
) inference.ProviderDefinition {
	t.Helper()
	driver, err := inference.BindGenerate(
		routeCompiler(),
		routeTransport(fail),
		decode,
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	return inference.ProviderDefinition{
		ID: id,
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{
				ID:           inference.ModelID{Provider: id, Name: "model-1"},
				Capabilities: inference.ModelCapabilities{Outputs: outputs},
			},
			Openers: inference.Openers{
				Generate: func(
					context.Context, inference.ModelRef,
				) (inference.GenerateOperations, error) {
					return inference.GenerateOperations{Unary: driver}, nil
				},
			},
		}},
	}
}

func newRouteAssembly(t *testing.T) *inference.Assembly {
	t.Helper()
	return assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.bad":  providerDefinition(t, "bad", true),
		"provider.good": providerDefinition(t, "good", false),
	})
}

func assemblyWithProviders(
	t *testing.T,
	providers map[string]inference.ProviderDefinition,
) *inference.Assembly {
	t.Helper()
	deps := make(map[string]any, len(providers))
	for name, definition := range providers {
		deps[name] = definition
	}
	factory := inference.Factory{}
	value, err := factory.New(context.Background(), resource.Input{
		Deps: deps,
	})
	if err != nil {
		t.Fatalf("build assembly: %v", err)
	}
	return value.(*inference.Assembly)
}

func routeRequest() inference.GenerateRequest {
	return inference.GenerateRequest{Input: inference.GenerateInput{
		Role: inference.InputRoleUser,
		Content: inference.InputContent{
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "hello"},
			}},
			Intent: inference.Intent{Text: &inference.TextIntent{}},
		},
	}}
}

func TestRouterGenerateFallsBackAcrossPools(t *testing.T) {
	assembly := newRouteAssembly(t)
	policy := Policy{
		Generate: []Pool{
			{Tier: "primary", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "bad", Name: "model-1"},
				},
			}}},
			{Tier: "fallback", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "good", Name: "model-1"},
				},
			}}},
		},
		Retry: &RetryConfig{
			Generate: &RetryPolicyConfig{
				MaxAttempts:              1,
				Retryable:                []RetryableClass{RetryableUnavailable},
				FallbackOnRetryExhausted: true,
			},
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(assembly, policy.Selectors(assembly), options...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, trace, err := router.Generate(context.Background(), routeRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want fallback response", response.Usage)
	}
	if len(trace.Fallbacks) != 1 ||
		trace.Fallbacks[0].From.ID.Provider != "bad" ||
		trace.Fallbacks[0].To.ID.Provider != "good" {
		t.Fatalf("fallbacks = %+v", trace.Fallbacks)
	}
}

func TestFactoryBuildsRouterFromSettings(t *testing.T) {
	assembly := newRouteAssembly(t)
	factory := Factory{}
	value, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{"target": assembly},
		Settings: []byte(`{
			"generate": [
				{"tier": "primary", "targets": [{"model": {"id": {"provider": "bad", "name": "model-1"}}}]},
				{"tier": "fallback", "targets": [{"model": {"id": {"provider": "good", "name": "model-1"}}}]}
			],
			"retry": {
				"generate": {
					"max_attempts": 1,
					"retryable": ["unavailable"],
					"fallback_on_retry_exhausted": true
				}
			}
		}`),
	})
	if err != nil {
		t.Fatalf("Factory.New: %v", err)
	}
	router, ok := value.(*Router)
	if !ok {
		t.Fatalf("Factory.New returned %T, want *Router", value)
	}
	response, _, err := router.Generate(context.Background(), routeRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want 7", response.Usage)
	}
}

func transcriptionProviderDefinition(
	t *testing.T,
	id string,
	fail bool,
) inference.ProviderDefinition {
	t.Helper()
	return transcriptionProviderWithTransports(
		t,
		id,
		routeTransport(fail),
		routeSessionTransport(fail),
	)
}

// transcriptionProviderWithTransports builds a transcription provider
// definition with caller-supplied unary and session transports, so tests can
// inject retry/circuit behavior per attempt.
func transcriptionProviderWithTransports(
	t *testing.T,
	id string,
	unaryTransport inference.Transport[routeWire, routeRaw],
	sessionTransport inference.TranscriptionSessionTransport[routeWire, routeRawEvent],
) inference.ProviderDefinition {
	t.Helper()
	unary, err := inference.BindTranscribe(
		inference.Compiler[inference.TranscriptionRequest, routeWire](
			func(_ context.Context, _ inference.ModelRef, request inference.TranscriptionRequest) (inference.Compiled[routeWire], error) {
				return routeTranscriptionCompiled(request.ActiveFields())
			},
		),
		unaryTransport,
		inference.Decoder[routeRaw, inference.TranscriptionResponse](
			func(_ context.Context, raw routeRaw) (inference.TranscriptionResponse, error) {
				return inference.TranscriptionResponse{
					Text:     raw.Text,
					Segments: []inference.TranscriptionSegment{{Text: raw.Text}},
				}, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindTranscribe: %v", err)
	}
	session, err := inference.BindTranscribeSession(
		inference.Compiler[inference.TranscriptionSessionRequest, routeWire](
			func(_ context.Context, _ inference.ModelRef, request inference.TranscriptionSessionRequest) (inference.Compiled[routeWire], error) {
				return routeTranscriptionCompiled(request.ActiveFields())
			},
		),
		sessionTransport,
		inference.TranscriptionSessionDecoder[routeRawEvent](
			func(_ context.Context, event routeRawEvent) (inference.TranscriptionSessionEvent, error) {
				return inference.TranscriptionSessionEvent{
					Text:  event.Text,
					Final: event.Final,
				}, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindTranscribeSession: %v", err)
	}
	return inference.ProviderDefinition{
		ID: id,
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: id, Name: "model-1"},
			},
			Openers: inference.Openers{
				Transcribe: func(
					_ context.Context,
					_ inference.ModelRef,
				) (inference.TranscribeOperations, error) {
					return inference.TranscribeOperations{
						Unary:   unary,
						Session: session,
					}, nil
				},
			},
		}},
	}
}

func routeTranscriptionCompiled(
	active []inference.FieldID,
) (inference.Compiled[routeWire], error) {
	decisions := make([]inference.Decision, len(active))
	for index, field := range active {
		decisions[index] = inference.Decision{
			Field:       field,
			Disposition: inference.Native,
		}
	}
	return inference.Compiled[routeWire]{
		Wire: routeWire{},
		Report: inference.CompileReport{
			Operation: inference.OperationTranscription,
			Decisions: decisions,
		},
	}, nil
}

type routeRawEvent struct {
	Text  string
	Final bool
}

type routeRawSession struct {
	events []routeRawEvent
}

func (s *routeRawSession) Send(
	context.Context,
	media.AudioChunk,
) error {
	return nil
}

func (s *routeRawSession) Next(context.Context) (routeRawEvent, error) {
	if len(s.events) == 0 {
		return routeRawEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (s *routeRawSession) Interrupt() error { return nil }
func (s *routeRawSession) Close() error     { return nil }

func routeSessionTransport(
	fail bool,
) inference.TranscriptionSessionTransport[routeWire, routeRawEvent] {
	return func(
		context.Context,
		routeWire,
	) (inference.ProviderSession[routeRawEvent], error) {
		if fail {
			return nil, errdefs.NotAvailablef("upstream unavailable")
		}
		return &routeRawSession{
			events: []routeRawEvent{{Text: "ok", Final: true}},
		}, nil
	}
}

// transcribeUnaryTransport fails the first failures calls with a transient
// error, then succeeds, so tests can exercise same-target retry.
func transcribeUnaryTransport(
	failures int,
) inference.Transport[routeWire, routeRaw] {
	var mu sync.Mutex
	calls := 0
	return func(context.Context, routeWire) (routeRaw, error) {
		mu.Lock()
		calls++
		fail := calls <= failures
		mu.Unlock()
		if fail {
			return routeRaw{}, errdefs.NotAvailablef("upstream unavailable")
		}
		return routeRaw{Text: "ok"}, nil
	}
}

// transcribeSessionTransport fails the first failures opens with a transient
// error, then opens a session.
func transcribeSessionTransport(
	failures int,
) inference.TranscriptionSessionTransport[routeWire, routeRawEvent] {
	var mu sync.Mutex
	calls := 0
	return func(
		context.Context,
		routeWire,
	) (inference.ProviderSession[routeRawEvent], error) {
		mu.Lock()
		calls++
		fail := calls <= failures
		mu.Unlock()
		if fail {
			return nil, errdefs.NotAvailablef("upstream unavailable")
		}
		return &routeRawSession{
			events: []routeRawEvent{{Text: "ok", Final: true}},
		}, nil
	}
}

func routeTranscriptionRequest() inference.TranscriptionRequest {
	source, err := media.NewAudioBytes([]byte{1, 2, 3, 4}, "audio/wav")
	if err != nil {
		panic(err)
	}
	return inference.TranscriptionRequest{Audio: source}
}

func routeTranscriptionSessionRequest() inference.TranscriptionSessionRequest {
	return inference.TranscriptionSessionRequest{
		InputFormat: media.AudioFormat{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 16000,
			Channels:     1,
		},
	}
}

func transcriptionAssemblyWith(
	t *testing.T,
	definitions map[string]inference.ProviderDefinition,
) *inference.Assembly {
	t.Helper()
	deps := make(map[string]any, len(definitions))
	for name, definition := range definitions {
		deps["provider."+name] = definition
	}
	value, err := inference.Factory{}.New(context.Background(), resource.Input{
		Deps: deps,
	})
	if err != nil {
		t.Fatalf("build transcription assembly: %v", err)
	}
	return value.(*inference.Assembly)
}

// noopSleeper replaces the retry backoff sleeper so resilience tests run
// without real delays. Tests live in the route package and can reach the
// unexported routerOptions.
func noopSleeper() Option {
	return func(configured *routerOptions) error {
		configured.sleeper = func(context.Context, time.Duration) error {
			return nil
		}
		return nil
	}
}

func newTranscriptionRouteAssembly(t *testing.T) *inference.Assembly {
	t.Helper()
	return transcriptionAssemblyWith(t, map[string]inference.ProviderDefinition{
		"bad":  transcriptionProviderDefinition(t, "bad", true),
		"good": transcriptionProviderDefinition(t, "good", false),
	})
}

func TestPolicyTranscriptionPools(t *testing.T) {
	policy := Policy{
		Transcription: []Pool{{
			Tier: "primary",
			Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "good", Name: "model-1"},
				},
			}},
		}},
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := (Policy{}).Validate(); err == nil {
		t.Fatal("empty policy validated")
	}
}

func TestRouterTranscribeFallsBackAcrossPools(t *testing.T) {
	assembly := newTranscriptionRouteAssembly(t)
	policy := Policy{
		Transcription: []Pool{
			{Tier: "primary", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "bad", Name: "model-1"},
				},
			}}},
			{Tier: "fallback", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "good", Name: "model-1"},
				},
			}}},
		},
		Retry: &RetryConfig{
			Transcription: &RetryPolicyConfig{
				MaxAttempts:              1,
				Retryable:                []RetryableClass{RetryableUnavailable},
				FallbackOnRetryExhausted: true,
			},
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(assembly, policy.Selectors(assembly), options...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, trace, err := router.Transcribe(
		context.Background(),
		routeTranscriptionRequest(),
	)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if response.Text != "ok" {
		t.Fatalf("Text = %q, want fallback response", response.Text)
	}
	if len(trace.Fallbacks) != 1 ||
		trace.Fallbacks[0].From.ID.Provider != "bad" ||
		trace.Fallbacks[0].To.ID.Provider != "good" {
		t.Fatalf("fallbacks = %+v", trace.Fallbacks)
	}
}

func TestRouterTranscribeSessionOpens(t *testing.T) {
	assembly := newTranscriptionRouteAssembly(t)
	policy := Policy{
		Transcription: []Pool{{
			Tier: "primary",
			Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "good", Name: "model-1"},
				},
			}},
		}},
	}
	router, err := New(assembly, policy.Selectors(assembly))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, _, err := router.TranscribeSession(
		context.Background(),
		routeTranscriptionSessionRequest(),
	)
	if err != nil {
		t.Fatalf("TranscribeSession: %v", err)
	}
	if err := session.Send(
		context.Background(),
		media.AudioChunk{Data: []byte{0, 0}},
	); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for {
		_, err := session.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	response, err := session.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if response.Text != "ok" {
		t.Fatalf("Text = %q, want %q", response.Text, "ok")
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestFactoryBuildsRouterWithTranscription(t *testing.T) {
	assembly := newTranscriptionRouteAssembly(t)
	factory := Factory{}
	value, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{"target": assembly},
		Settings: []byte(`{
			"transcription": [
				{"tier": "primary", "targets": [{"model": {"id": {"provider": "good", "name": "model-1"}}}]}
			],
			"retry": {
				"transcription": {"max_attempts": 1}
			}
		}`),
	})
	if err != nil {
		t.Fatalf("Factory.New: %v", err)
	}
	router, ok := value.(*Router)
	if !ok {
		t.Fatalf("Factory.New returned %T, want *Router", value)
	}
	response, _, err := router.Transcribe(
		context.Background(),
		routeTranscriptionRequest(),
	)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if response.Text != "ok" {
		t.Fatalf("Text = %q, want %q", response.Text, "ok")
	}
}

func TestRouterTranscribeStream(t *testing.T) {
	assembly := newTranscriptionRouteAssembly(t)
	policy := Policy{
		Transcription: []Pool{{
			Tier: "primary",
			Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "good", Name: "model-1"},
				},
			}},
		}},
	}
	router, err := New(assembly, policy.Selectors(assembly))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	source, err := media.NewAudioBytes([]byte{0, 0}, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	pipe := message.NewPartPipe(1)
	pipe.Send(message.AudioPart{Source: source})
	pipe.Close()
	response, trace, err := router.TranscribeStream(
		context.Background(),
		routeTranscriptionSessionRequest(),
		pipe,
	)
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	if response.Text != "ok" {
		t.Fatalf("Text = %q, want %q", response.Text, "ok")
	}
	if trace.Executed.ID.Provider != "good" {
		t.Fatalf("trace executed = %+v, want good provider", trace.Executed)
	}
}

func TestRouterTranscribeRetriesSameTarget(t *testing.T) {
	retryRef := inference.ModelRef{
		ID: inference.ModelID{Provider: "retry", Name: "model-1"},
	}
	assembly := transcriptionAssemblyWith(t, map[string]inference.ProviderDefinition{
		"retry": transcriptionProviderWithTransports(
			t,
			"retry",
			transcribeUnaryTransport(1),
			routeSessionTransport(false),
		),
	})
	policy := Policy{
		Transcription: []Pool{{
			Tier:    "primary",
			Targets: []Target{{Model: retryRef}},
		}},
		Retry: &RetryConfig{
			Transcription: &RetryPolicyConfig{
				MaxAttempts: 2,
				Retryable:   []RetryableClass{RetryableUnavailable},
			},
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(
		assembly,
		policy.Selectors(assembly),
		append(options, noopSleeper())...,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, trace, err := router.Transcribe(
		context.Background(), routeTranscriptionRequest(),
	)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if response.Text != "ok" {
		t.Fatalf("Text = %q, want %q", response.Text, "ok")
	}
	if len(trace.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(trace.Attempts))
	}
	first, second := trace.Attempts[0], trace.Attempts[1]
	if first.Outcome != AttemptOutcomeFailed ||
		first.Trigger != AttemptTriggerSelection ||
		!first.Transient {
		t.Fatalf("first attempt = %+v, want transient failed selection", first)
	}
	if second.Outcome != AttemptOutcomeSucceeded ||
		second.Trigger != AttemptTriggerRetry {
		t.Fatalf("second attempt = %+v, want retried success", second)
	}
	if trace.Executed != retryRef {
		t.Fatalf("executed = %+v, want retry target", trace.Executed)
	}
}

func TestRouterTranscribeCircuitBreakerSkipsOpenTarget(t *testing.T) {
	badRef := inference.ModelRef{
		ID: inference.ModelID{Provider: "bad", Name: "model-1"},
	}
	assembly := newTranscriptionRouteAssembly(t)
	policy := Policy{
		Transcription: []Pool{{
			Tier:    "primary",
			Targets: []Target{{Model: badRef}},
		}},
		CircuitBreaker: &CircuitBreakerConfig{
			FailureThreshold: 1,
			RecoveryWindow:   time.Hour,
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(
		assembly,
		policy.Selectors(assembly),
		append(options, noopSleeper())...,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, trace, err := router.Transcribe(
		context.Background(), routeTranscriptionRequest(),
	)
	if err == nil {
		t.Fatal("first Transcribe succeeded against a failing target")
	}
	if len(trace.Attempts) != 1 ||
		trace.Attempts[0].Outcome != AttemptOutcomeFailed ||
		trace.Attempts[0].CircuitTransition != "open" {
		t.Fatalf("first attempts = %+v, want failure that opens the circuit",
			trace.Attempts)
	}

	_, trace, err = router.Transcribe(
		context.Background(), routeTranscriptionRequest(),
	)
	if !IsKind(err, CircuitOpen) {
		t.Fatalf("second Transcribe = %v, want circuit-open rejection", err)
	}
	if len(trace.Attempts) != 1 ||
		trace.Attempts[0].Outcome != AttemptOutcomeSkipped ||
		trace.Attempts[0].Circuit != "open" {
		t.Fatalf("second attempts = %+v, want circuit-open skip", trace.Attempts)
	}
}

func TestRouterGenerateSelectsCapableTargetForIntent(t *testing.T) {
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.text": providerDefinitionWithOutputs(
			t, "text", false,
			[]message.PartKind{message.PartText},
			routeDecode(),
		),
		"provider.image": providerDefinitionWithOutputs(
			t, "image", false,
			[]message.PartKind{message.PartImage},
			imageRouteDecode(),
		),
	})
	policy := Policy{
		Generate: []Pool{
			{Tier: "text", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "text", Name: "model-1"},
				},
			}}},
			{Tier: "image", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "image", Name: "model-1"},
				},
			}}},
		},
	}
	router, err := New(assembly, policy.Selectors(assembly))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	textResponse, textTrace, err := router.Generate(
		context.Background(), routeRequest(),
	)
	if err != nil {
		t.Fatalf("text Generate: %v", err)
	}
	if textResponse.Usage.TotalTokens != 7 {
		t.Fatalf("text usage = %+v", textResponse.Usage)
	}
	if textTrace.Executed.ID.Provider != "text" {
		t.Fatalf("text executed = %+v, want the text model", textTrace.Executed)
	}

	imageRequest := routeRequest()
	imageRequest.Input.Content.Intent = inference.Intent{Image: &inference.ImageIntent{}}
	imageResponse, imageTrace, err := router.Generate(
		context.Background(), imageRequest,
	)
	if err != nil {
		t.Fatalf("image Generate: %v", err)
	}
	if imageResponse.Usage.TotalTokens != 7 {
		t.Fatalf("image usage = %+v", imageResponse.Usage)
	}
	if imageTrace.Executed.ID.Provider != "image" {
		t.Fatalf(
			"image executed = %+v, want the image model without fallback",
			imageTrace.Executed,
		)
	}
	if len(imageTrace.Fallbacks) != 0 {
		t.Fatalf("image fallbacks = %+v, want none", imageTrace.Fallbacks)
	}
}

func TestRouterGenerateUndeclaredOutputsKeepsDeclaredOrder(t *testing.T) {
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.first":  providerDefinition(t, "first", false),
		"provider.second": providerDefinition(t, "second", false),
	})
	policy := Policy{
		Generate: []Pool{
			{Tier: "primary", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "first", Name: "model-1"},
				},
			}}},
			{Tier: "fallback", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "second", Name: "model-1"},
				},
			}}},
		},
	}
	router, err := New(assembly, policy.Selectors(assembly))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, trace, err := router.Generate(context.Background(), routeRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if trace.Executed.ID.Provider != "first" {
		t.Fatalf(
			"executed = %+v, want the first declared target (undeclared outputs do not filter)",
			trace.Executed,
		)
	}
}

func TestRouterGenerateDedupsRepeatedModelsAcrossTiers(t *testing.T) {
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.bad": providerDefinition(t, "bad", true),
		"provider.good": providerDefinitionWithOutputs(
			t, "good", false,
			[]message.PartKind{message.PartText},
			routeDecode(),
		),
	})
	repeated := inference.ModelRef{
		ID: inference.ModelID{Provider: "bad", Name: "model-1"},
	}
	good := inference.ModelRef{
		ID: inference.ModelID{Provider: "good", Name: "model-1"},
	}
	policy := Policy{
		Generate: []Pool{
			{Tier: "high", Targets: []Target{{Model: repeated}}},
			{Tier: "medium", Targets: []Target{{Model: repeated}}},
			{Tier: "low", Targets: []Target{{Model: repeated}}},
			{Tier: "image", Targets: []Target{{Model: good}}},
		},
		Retry: &RetryConfig{
			Generate: &RetryPolicyConfig{
				MaxAttempts:              1,
				Retryable:                []RetryableClass{RetryableUnavailable},
				FallbackOnRetryExhausted: true,
			},
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(assembly, policy.Selectors(assembly), options...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, trace, err := router.Generate(context.Background(), routeRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want fallback response", response.Usage)
	}
	if len(trace.Fallbacks) != 1 ||
		trace.Fallbacks[0].From.ID.Provider != "bad" ||
		trace.Fallbacks[0].To.ID.Provider != "good" {
		t.Fatalf(
			"fallbacks = %+v, want one hop from the repeated model to the distinct target",
			trace.Fallbacks,
		)
	}
	if trace.Executed.ID.Provider != "good" {
		t.Fatalf(
			"executed = %+v, want the distinct good target",
			trace.Executed,
		)
	}
	badExecutions := 0
	for _, attempt := range trace.Attempts {
		if attempt.Phase != AttemptPhaseExecute {
			continue
		}
		if attempt.Target.ID.Provider == "bad" {
			badExecutions++
		}
	}
	if badExecutions != 1 {
		t.Fatalf(
			"bad model executed %d times, want exactly once (duplicates collapsed)",
			badExecutions,
		)
	}
}

func TestRouterGenerateNoRouteWhenNoDeclaredTargetServesIntent(t *testing.T) {
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.text": providerDefinitionWithOutputs(
			t, "text", false,
			[]message.PartKind{message.PartText},
			routeDecode(),
		),
	})
	policy := Policy{
		Generate: []Pool{
			{Tier: "text", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "text", Name: "model-1"},
				},
			}}},
		},
	}
	router, err := New(assembly, policy.Selectors(assembly))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := routeRequest()
	request.Input.Content.Intent = inference.Intent{Image: &inference.ImageIntent{}}
	if _, _, err := router.Generate(context.Background(), request); !IsKind(err, NoRoute) {
		t.Fatalf("Generate = %v, want no-route", err)
	}
}

func TestRouterTranscribeSessionRetriesOpen(t *testing.T) {
	retryRef := inference.ModelRef{
		ID: inference.ModelID{Provider: "retry", Name: "model-1"},
	}
	assembly := transcriptionAssemblyWith(t, map[string]inference.ProviderDefinition{
		"retry": transcriptionProviderWithTransports(
			t,
			"retry",
			routeTransport(false),
			transcribeSessionTransport(1),
		),
	})
	policy := Policy{
		Transcription: []Pool{{
			Tier:    "primary",
			Targets: []Target{{Model: retryRef}},
		}},
		Retry: &RetryConfig{
			Transcription: &RetryPolicyConfig{
				MaxAttempts: 2,
				Retryable:   []RetryableClass{RetryableUnavailable},
			},
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(
		assembly,
		policy.Selectors(assembly),
		append(options, noopSleeper())...,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, trace, err := router.TranscribeSession(
		context.Background(), routeTranscriptionSessionRequest(),
	)
	if err != nil {
		t.Fatalf("TranscribeSession: %v", err)
	}
	if session == nil {
		t.Fatal("TranscribeSession returned a nil session")
	}
	// Each logical open attempt re-runs the session preflight, so one
	// retry yields four attempts: preflight+open per logical attempt.
	if len(trace.Attempts) != 4 {
		t.Fatalf("attempts = %d, want 4", len(trace.Attempts))
	}
	first, second := trace.Attempts[1], trace.Attempts[3]
	if first.Outcome != AttemptOutcomeFailed ||
		first.Phase != AttemptPhaseOpen ||
		!first.Transient {
		t.Fatalf("first attempt = %+v, want transient open failure", first)
	}
	if second.Outcome != AttemptOutcomeOpened ||
		second.Trigger != AttemptTriggerRetry {
		t.Fatalf("second attempt = %+v, want retried open", second)
	}
	if trace.Executed != retryRef {
		t.Fatalf("executed = %+v, want retry target", trace.Executed)
	}
}

func TestRouterTranscribeSessionFallsBackAfterRetryExhausted(t *testing.T) {
	badRef := inference.ModelRef{
		ID: inference.ModelID{Provider: "bad", Name: "model-1"},
	}
	goodRef := inference.ModelRef{
		ID: inference.ModelID{Provider: "good", Name: "model-1"},
	}
	assembly := newTranscriptionRouteAssembly(t)
	policy := Policy{
		Transcription: []Pool{
			{Tier: "primary", Targets: []Target{{Model: badRef}}},
			{Tier: "fallback", Targets: []Target{{Model: goodRef}}},
		},
		Retry: &RetryConfig{
			Transcription: &RetryPolicyConfig{
				MaxAttempts:              1,
				Retryable:                []RetryableClass{RetryableUnavailable},
				FallbackOnRetryExhausted: true,
			},
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(
		assembly,
		policy.Selectors(assembly),
		append(options, noopSleeper())...,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, trace, err := router.TranscribeSession(
		context.Background(), routeTranscriptionSessionRequest(),
	)
	if err != nil {
		t.Fatalf("TranscribeSession: %v", err)
	}
	if session == nil {
		t.Fatal("TranscribeSession returned a nil session")
	}
	if len(trace.Fallbacks) != 1 ||
		trace.Fallbacks[0].From != badRef ||
		trace.Fallbacks[0].To != goodRef {
		t.Fatalf("fallbacks = %+v, want bad -> good", trace.Fallbacks)
	}
	if trace.Executed != goodRef {
		t.Fatalf("executed = %+v, want fallback target", trace.Executed)
	}
}

func TestRouterTranscribeSessionCircuitBreakerSkipsOpenTarget(t *testing.T) {
	badRef := inference.ModelRef{
		ID: inference.ModelID{Provider: "bad", Name: "model-1"},
	}
	assembly := newTranscriptionRouteAssembly(t)
	policy := Policy{
		Transcription: []Pool{{
			Tier:    "primary",
			Targets: []Target{{Model: badRef}},
		}},
		CircuitBreaker: &CircuitBreakerConfig{
			FailureThreshold: 1,
			RecoveryWindow:   time.Hour,
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(
		assembly,
		policy.Selectors(assembly),
		append(options, noopSleeper())...,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, trace, err := router.TranscribeSession(
		context.Background(), routeTranscriptionSessionRequest(),
	)
	if err == nil {
		t.Fatal("first TranscribeSession succeeded against a failing target")
	}
	if len(trace.Attempts) != 2 {
		t.Fatalf("first attempts = %d, want 2 (preflight + open)",
			len(trace.Attempts))
	}
	openAttempt := trace.Attempts[1]
	if openAttempt.Outcome != AttemptOutcomeFailed ||
		openAttempt.CircuitTransition != "open" {
		t.Fatalf("first attempts = %+v, want failure that opens the circuit",
			trace.Attempts)
	}

	_, trace, err = router.TranscribeSession(
		context.Background(), routeTranscriptionSessionRequest(),
	)
	if !IsKind(err, CircuitOpen) {
		t.Fatalf("second TranscribeSession = %v, want circuit-open rejection", err)
	}
	if len(trace.Attempts) != 1 ||
		trace.Attempts[0].Outcome != AttemptOutcomeSkipped ||
		trace.Attempts[0].Circuit != "open" {
		t.Fatalf("second attempts = %+v, want circuit-open skip", trace.Attempts)
	}
}

func providerDefinitionNamed(
	t *testing.T,
	id, name string,
	fail bool,
	outputs []message.PartKind,
) inference.ProviderDefinition {
	t.Helper()
	driver, err := inference.BindGenerate(
		routeCompiler(),
		routeTransport(fail),
		routeDecode(),
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	return inference.ProviderDefinition{
		ID: id,
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: id, Name: name},
				Capabilities: inference.ModelCapabilities{
					Outputs: outputs,
				},
			},
			Openers: inference.Openers{
				Generate: func(
					context.Context, inference.ModelRef,
				) (inference.GenerateOperations, error) {
					return inference.GenerateOperations{Unary: driver}, nil
				},
			},
		}},
	}
}

func hintedRequest(hint string) inference.GenerateRequest {
	request := routeRequest()
	request.ModelHint = hint
	return request
}

func TestPolicyGenerateHintSelectsConfiguredTarget(t *testing.T) {
	good := inference.ModelRef{
		ID:      inference.ModelID{Provider: "good", Name: "model-1"},
		Profile: "prod",
	}
	policy := Policy{Generate: []Pool{
		{Tier: "primary", Targets: []Target{{Model: inference.ModelRef{
			ID: inference.ModelID{Provider: "bad", Name: "model-1"},
		}}}},
		{Tier: "fallback", Targets: []Target{{Model: good}}},
	}}
	selectors := policy.Selectors(nil)
	decision, err := selectors.Generate.SelectGenerate(
		context.Background(),
		hintedRequest("good/model-1"),
	)
	if err != nil {
		t.Fatalf("SelectGenerate: %v", err)
	}
	if decision.Selected != good {
		t.Fatalf("Selected = %+v, want hinted target %+v", decision.Selected, good)
	}
	if decision.Tier != "fallback" {
		t.Fatalf("Tier = %q, want hinted target's tier", decision.Tier)
	}
}

func TestPolicyGenerateHintUnknownOrEmptyFallsBack(t *testing.T) {
	first := inference.ModelRef{
		ID: inference.ModelID{Provider: "bad", Name: "model-1"},
	}
	policy := Policy{Generate: []Pool{
		{Tier: "primary", Targets: []Target{{Model: first}}},
		{Tier: "fallback", Targets: []Target{{Model: inference.ModelRef{
			ID: inference.ModelID{Provider: "good", Name: "model-1"},
		}}}},
	}}
	selectors := policy.Selectors(nil)
	for _, hint := range []string{"", "nope/model-1", "bad/model-2", "/model-1", "bad/", "a/b/c"} {
		decision, err := selectors.Generate.SelectGenerate(
			context.Background(),
			hintedRequest(hint),
		)
		if err != nil {
			t.Fatalf("hint %q: SelectGenerate: %v", hint, err)
		}
		if decision.Selected != first {
			t.Fatalf("hint %q: Selected = %+v, want default %+v", hint, decision.Selected, first)
		}
	}
}

func TestPolicyGenerateHintBareNameUniqueAndAmbiguous(t *testing.T) {
	first := inference.ModelRef{
		ID: inference.ModelID{Provider: "provider-a", Name: "model-a"},
	}
	second := inference.ModelRef{
		ID: inference.ModelID{Provider: "provider-b", Name: "model-b"},
	}
	policy := Policy{Generate: []Pool{{
		Tier: "primary",
		Targets: []Target{
			{Model: first},
			{Model: second},
		},
	}}}
	selectors := policy.Selectors(nil)

	decision, err := selectors.Generate.SelectGenerate(
		context.Background(),
		hintedRequest("model-b"),
	)
	if err != nil {
		t.Fatalf("SelectGenerate(bare unique): %v", err)
	}
	if decision.Selected != second {
		t.Fatalf("bare hint Selected = %+v, want %+v", decision.Selected, second)
	}

	ambiguous := Policy{Generate: []Pool{{
		Tier: "primary",
		Targets: []Target{
			{Model: inference.ModelRef{
				ID: inference.ModelID{Provider: "provider-a", Name: "shared"},
			}},
			{Model: inference.ModelRef{
				ID: inference.ModelID{Provider: "provider-b", Name: "shared"},
			}},
		},
	}}}
	ambiguousSelectors := ambiguous.Selectors(nil)
	decision, err = ambiguousSelectors.Generate.SelectGenerate(
		context.Background(),
		hintedRequest("shared"),
	)
	if err != nil {
		t.Fatalf("SelectGenerate(ambiguous): %v", err)
	}
	wantFirst := inference.ModelRef{
		ID: inference.ModelID{Provider: "provider-a", Name: "shared"},
	}
	if decision.Selected != wantFirst {
		t.Fatalf("ambiguous bare hint Selected = %+v, want first target", decision.Selected)
	}
}

func TestPolicyGenerateHintTargetUnsupportedOutputsFallsBack(t *testing.T) {
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.image": providerDefinitionNamed(t, "image-only", "model-1", false, []message.PartKind{message.PartImage}),
		"provider.text":  providerDefinitionNamed(t, "text-only", "model-1", false, []message.PartKind{message.PartText}),
	})
	textRef := inference.ModelRef{
		ID: inference.ModelID{Provider: "text-only", Name: "model-1"},
	}
	policy := Policy{Generate: []Pool{{
		Tier: "primary",
		Targets: []Target{
			{Model: inference.ModelRef{
				ID: inference.ModelID{Provider: "image-only", Name: "model-1"},
			}},
			{Model: textRef},
		},
	}}}
	selectors := policy.Selectors(assembly)
	decision, err := selectors.Generate.SelectGenerate(
		context.Background(),
		hintedRequest("image-only/model-1"),
	)
	if err != nil {
		t.Fatalf("SelectGenerate: %v", err)
	}
	if decision.Selected != textRef {
		t.Fatalf("Selected = %+v, want capability-compatible fallback %+v",
			decision.Selected, textRef)
	}
}

func TestRouterGenerateHintSelectsTargetWithoutFallback(t *testing.T) {
	assembly := newRouteAssembly(t)
	goodRef := inference.ModelRef{
		ID: inference.ModelID{Provider: "good", Name: "model-1"},
	}
	policy := Policy{
		Generate: []Pool{
			{Tier: "primary", Targets: []Target{{Model: inference.ModelRef{
				ID: inference.ModelID{Provider: "bad", Name: "model-1"},
			}}}},
			{Tier: "fallback", Targets: []Target{{Model: goodRef}}},
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(assembly, policy.Selectors(assembly), options...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, trace, err := router.Generate(
		context.Background(),
		hintedRequest("good/model-1"),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want hinted target response", response.Usage)
	}
	if len(trace.Fallbacks) != 0 {
		t.Fatalf("fallbacks = %+v, want none (hint picked the good target first)", trace.Fallbacks)
	}
	if trace.Executed != goodRef {
		t.Fatalf("executed = %+v, want %+v", trace.Executed, goodRef)
	}
}

func hintFallbackRetryConfig() *RetryConfig {
	return &RetryConfig{
		Generate: &RetryPolicyConfig{
			MaxAttempts:              1,
			Retryable:                []RetryableClass{RetryableUnavailable},
			FallbackOnRetryExhausted: true,
		},
	}
}

func TestRouterGenerateHintFallsBackToDefaultChain(t *testing.T) {
	goodA := inference.ModelRef{
		ID: inference.ModelID{Provider: "good-a", Name: "model-1"},
	}
	failB := inference.ModelRef{
		ID: inference.ModelID{Provider: "fail-b", Name: "model-1"},
	}
	failC := inference.ModelRef{
		ID: inference.ModelID{Provider: "fail-c", Name: "model-1"},
	}
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.good-a": providerDefinitionNamed(t, "good-a", "model-1", false, nil),
		"provider.fail-b": providerDefinitionNamed(t, "fail-b", "model-1", true, nil),
		"provider.fail-c": providerDefinitionNamed(t, "fail-c", "model-1", true, nil),
	})
	policy := Policy{
		Generate: []Pool{
			{Tier: "primary", Targets: []Target{{Model: goodA}}},
			{Tier: "fallback", Targets: []Target{{Model: failB}, {Model: failC}}},
		},
		Retry: hintFallbackRetryConfig(),
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(assembly, policy.Selectors(assembly), options...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Hint picks C (the last declared target). C fails, and fallback must
	// restart at the head of the declared order (A), skipping B — it must
	// not fail just because C was last.
	response, trace, err := router.Generate(
		context.Background(),
		hintedRequest("fail-c/model-1"),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want default-chain response", response.Usage)
	}
	if trace.Executed != goodA {
		t.Fatalf("executed = %+v, want %+v", trace.Executed, goodA)
	}
	if len(trace.Fallbacks) != 1 ||
		trace.Fallbacks[0].From != failC ||
		trace.Fallbacks[0].To != goodA {
		t.Fatalf("fallbacks = %+v, want C -> A", trace.Fallbacks)
	}
	attempts := trace.Attempts
	wantAttempts := []struct {
		target  inference.ModelRef
		phase   AttemptPhase
		outcome AttemptOutcome
	}{
		{failC, AttemptPhasePreflight, AttemptOutcomeSucceeded},
		{failC, AttemptPhaseExecute, AttemptOutcomeFailed},
		{goodA, AttemptPhasePreflight, AttemptOutcomeSucceeded},
		{goodA, AttemptPhaseExecute, AttemptOutcomeSucceeded},
	}
	if len(attempts) != len(wantAttempts) {
		t.Fatalf("attempts = %+v, want %+v", attempts, wantAttempts)
	}
	for index, want := range wantAttempts {
		if attempts[index].Target != want.target ||
			attempts[index].Phase != want.phase ||
			attempts[index].Outcome != want.outcome {
			t.Fatalf("attempts = %+v, want %+v", attempts, wantAttempts)
		}
	}
}

func TestRouterGenerateHintFallbackNeverRevisitsHintedTarget(t *testing.T) {
	failA := inference.ModelRef{
		ID: inference.ModelID{Provider: "fail-a", Name: "model-1"},
	}
	goodB := inference.ModelRef{
		ID: inference.ModelID{Provider: "good-b", Name: "model-1"},
	}
	failC := inference.ModelRef{
		ID: inference.ModelID{Provider: "fail-c", Name: "model-1"},
	}
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.fail-a": providerDefinitionNamed(t, "fail-a", "model-1", true, nil),
		"provider.good-b": providerDefinitionNamed(t, "good-b", "model-1", false, nil),
		"provider.fail-c": providerDefinitionNamed(t, "fail-c", "model-1", true, nil),
	})
	policy := Policy{
		Generate: []Pool{{
			Tier: "primary",
			Targets: []Target{
				{Model: failA},
				{Model: goodB},
				{Model: failC},
			},
		}},
		Retry: hintFallbackRetryConfig(),
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(assembly, policy.Selectors(assembly), options...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Hint picks C. C fails, then A fails; the chain stops at the healthy
	// B and the hinted C is never re-attempted.
	response, trace, err := router.Generate(
		context.Background(),
		hintedRequest("fail-c/model-1"),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want B response", response.Usage)
	}
	if trace.Executed != goodB {
		t.Fatalf("executed = %+v, want %+v", trace.Executed, goodB)
	}
	var targets []inference.ModelRef
	for _, attempt := range trace.Attempts {
		if attempt.Phase != AttemptPhaseExecute {
			continue
		}
		targets = append(targets, attempt.Target)
	}
	want := []inference.ModelRef{failC, failA, goodB}
	if len(targets) != len(want) {
		t.Fatalf("attempt targets = %v, want %v", targets, want)
	}
	for index := range want {
		if targets[index] != want[index] {
			t.Fatalf("attempt targets = %v, want %v", targets, want)
		}
	}
}

func TestRouterGenerateHintCircuitOpenFallsBackToDefaultChain(t *testing.T) {
	goodA := inference.ModelRef{
		ID: inference.ModelID{Provider: "good-a", Name: "model-1"},
	}
	failC := inference.ModelRef{
		ID: inference.ModelID{Provider: "fail-c", Name: "model-1"},
	}
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.good-a": providerDefinitionNamed(t, "good-a", "model-1", false, nil),
		"provider.fail-c": providerDefinitionNamed(t, "fail-c", "model-1", true, nil),
	})
	policy := Policy{
		Generate: []Pool{
			{Tier: "primary", Targets: []Target{{Model: goodA}}},
			{Tier: "fallback", Targets: []Target{{Model: failC}}},
		},
		Retry: hintFallbackRetryConfig(),
		CircuitBreaker: &CircuitBreakerConfig{
			FailureThreshold: 1,
			RecoveryWindow:   time.Hour,
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(
		assembly,
		policy.Selectors(assembly),
		append(options, noopSleeper())...,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First call: the hinted C fails and opens its circuit, fallback
	// restarts at A.
	if _, trace, err := router.Generate(
		context.Background(),
		hintedRequest("fail-c/model-1"),
	); err != nil {
		t.Fatalf("first Generate: %v", err)
	} else if trace.Executed != goodA ||
		len(trace.Fallbacks) != 1 ||
		trace.Fallbacks[0].From != failC {
		t.Fatalf("first trace = %+v, want C -> A", trace)
	}

	// Second call: C is circuit-open, so the hinted target is skipped and
	// A executes without touching C.
	_, trace, err := router.Generate(
		context.Background(),
		hintedRequest("fail-c/model-1"),
	)
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if trace.Executed != goodA {
		t.Fatalf("second executed = %+v, want %+v", trace.Executed, goodA)
	}
	if len(trace.Attempts) != 3 ||
		trace.Attempts[0].Target != failC ||
		trace.Attempts[0].Outcome != AttemptOutcomeSkipped ||
		trace.Attempts[0].Circuit != "open" ||
		trace.Attempts[1].Target != goodA ||
		trace.Attempts[2].Outcome != AttemptOutcomeSucceeded {
		t.Fatalf("second attempts = %+v, want C skipped (open) then A succeeded", trace.Attempts)
	}
}

type routeEventStream struct {
	events []inference.GenerateStreamEvent
	index  int
}

func newRouteEventStream() *routeEventStream {
	return &routeEventStream{events: []inference.GenerateStreamEvent{
		{PartIndex: 0, Delta: inference.TextPartDelta{Text: "ok"}},
		{FinishReason: inference.FinishCompleted},
	}}
}

func (s *routeEventStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	if s.index >= len(s.events) {
		return inference.GenerateStreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*routeEventStream) Close() error { return nil }

func streamProviderNamed(
	t *testing.T,
	id, name string,
) inference.ProviderDefinition {
	t.Helper()
	streamTransport := inference.Transport[routeWire, inference.ProviderStream[inference.GenerateStreamEvent]](
		func(context.Context, routeWire) (inference.ProviderStream[inference.GenerateStreamEvent], error) {
			return newRouteEventStream(), nil
		},
	)
	streamDecode := inference.GenerateStreamDecoder[inference.GenerateStreamEvent](
		func(_ context.Context, event inference.GenerateStreamEvent) (inference.GenerateStreamEvent, error) {
			return event, nil
		},
	)
	operations, err := inference.BindGenerateOperations(
		routeCompiler(),
		routeTransport(false),
		routeDecode(),
		streamTransport,
		streamDecode,
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	return inference.ProviderDefinition{
		ID: id,
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: id, Name: name},
			},
			Openers: inference.Openers{
				Generate: func(
					context.Context, inference.ModelRef,
				) (inference.GenerateOperations, error) {
					return operations, nil
				},
			},
		}},
	}
}

func TestRouterGenerateStreamHintFallsBackAfterPreflight(t *testing.T) {
	streamA := inference.ModelRef{
		ID: inference.ModelID{Provider: "stream-a", Name: "model-1"},
	}
	unaryB := inference.ModelRef{
		ID: inference.ModelID{Provider: "unary-b", Name: "model-1"},
	}
	assembly := assemblyWithProviders(t, map[string]inference.ProviderDefinition{
		"provider.stream-a": streamProviderNamed(t, "stream-a", "model-1"),
		"provider.unary-b":  providerDefinitionNamed(t, "unary-b", "model-1", false, nil),
	})
	policy := Policy{Generate: []Pool{
		{Tier: "primary", Targets: []Target{{Model: streamA}}},
		{Tier: "fallback", Targets: []Target{{Model: unaryB}}},
	}}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(assembly, policy.Selectors(assembly), options...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The hint names B, which has no stream opener: preflight fails with
	// UnsupportedOperation and fallback restarts at the head of the
	// declared order (A), which opens the stream.
	stream, trace, err := router.GenerateStream(
		context.Background(),
		hintedRequest("unary-b/model-1"),
	)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if trace.Executed != streamA {
		t.Fatalf("executed = %+v, want %+v", trace.Executed, streamA)
	}
	if len(trace.Fallbacks) != 1 ||
		trace.Fallbacks[0].From != unaryB ||
		trace.Fallbacks[0].To != streamA {
		t.Fatalf("fallbacks = %+v, want B -> A", trace.Fallbacks)
	}
	if len(trace.Attempts) != 3 ||
		trace.Attempts[0].Target != unaryB ||
		trace.Attempts[0].Phase != AttemptPhasePreflight ||
		trace.Attempts[0].Outcome != AttemptOutcomeFailed ||
		trace.Attempts[1].Target != streamA ||
		trace.Attempts[2].Outcome != AttemptOutcomeOpened {
		t.Fatalf("attempts = %+v, want B preflight failed, A preflight + opened", trace.Attempts)
	}
	defer func() { _ = stream.Close() }()
	event, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if event.PartIndex != 0 || event.Delta == nil {
		t.Fatalf("first event = %+v, want text delta", event)
	}
	for {
		if _, err := stream.Next(context.Background()); err == io.EOF {
			break
		}
	}
}
