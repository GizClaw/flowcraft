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

func providerDefinition(
	t *testing.T,
	id string,
	fail bool,
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
				ID: inference.ModelID{Provider: id, Name: "model-1"},
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
	factory := inference.Factory{}
	value, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{
			"provider.bad":  providerDefinition(t, "bad", true),
			"provider.good": providerDefinition(t, "good", false),
		},
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
	router, err := New(assembly, policy.Selectors(), options...)
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
	router, err := New(assembly, policy.Selectors(), options...)
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
	router, err := New(assembly, policy.Selectors())
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
	router, err := New(assembly, policy.Selectors())
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
		policy.Selectors(),
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
		policy.Selectors(),
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
		policy.Selectors(),
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
		policy.Selectors(),
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
		policy.Selectors(),
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
