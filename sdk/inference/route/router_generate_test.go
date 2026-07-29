package route

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

type generateSelectorFunc func(context.Context, inference.GenerateRequest) (Decision, error)

func (f generateSelectorFunc) SelectGenerate(
	ctx context.Context,
	request inference.GenerateRequest,
) (Decision, error) {
	return f(ctx, request)
}

type generateFallbackFunc func(
	context.Context,
	inference.GenerateRequest,
	Attempt,
) (inference.ModelRef, bool, error)

func (f generateFallbackFunc) NextGenerate(
	ctx context.Context,
	request inference.GenerateRequest,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return f(ctx, request, attempt)
}

func TestGenerateSelectorAndFallbackReceiveImmutableSnapshots(t *testing.T) {
	first := generateModel("first")
	second := generateModel("second")
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"first":  {reject: inference.UnsupportedFeature},
		"second": {},
	})
	fallbackCalls := 0
	router, err := New(runtime, Selectors{
		Generate: generateSelectorFunc(func(
			_ context.Context,
			request inference.GenerateRequest,
		) (Decision, error) {
			request.Input.Content.Parts[0] = inference.TextPart{Text: "selector mutation"}
			return generateDecision(first), nil
		}),
		GenerateFallback: generateFallbackFunc(func(
			_ context.Context,
			request inference.GenerateRequest,
			attempt Attempt,
		) (inference.ModelRef, bool, error) {
			fallbackCalls++
			request.Input.Content.Parts[0] = inference.TextPart{Text: "fallback mutation"}
			if attempt.Target != first || attempt.ErrorKind != inference.UnsupportedFeature {
				t.Fatalf("fallback attempt = %+v", attempt)
			}
			return second, true, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := generateRequest("original")
	response, trace, err := router.Generate(context.Background(), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if fallbackCalls != 1 || response.Metadata.Model != second.ID {
		t.Fatalf("fallback calls/model = %d/%+v", fallbackCalls, response.Metadata.Model)
	}
	if trace.Executed != second {
		t.Fatalf("trace executed = %+v, want %+v", trace.Executed, second)
	}
	if got := request.Input.Content.Parts[0].(inference.TextPart).Text; got != "original" {
		t.Fatalf("caller request mutated to %q", got)
	}
	if len(trace.Attempts) != 3 ||
		trace.Attempts[0].Phase != AttemptPhasePreflight ||
		trace.Attempts[2].Outcome != AttemptOutcomeSucceeded {
		t.Fatalf("trace attempts = %+v", trace.Attempts)
	}
}

func TestGenerateReturnsTerminalFailureTrace(t *testing.T) {
	first := generateModel("first")
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"first": {reject: inference.UnsupportedFeature},
	})
	router := newGenerateRouter(t, runtime, first, generateFallbackFunc(func(
		context.Context,
		inference.GenerateRequest,
		Attempt,
	) (inference.ModelRef, bool, error) {
		return inference.ModelRef{}, false, nil
	}))
	_, trace, err := router.Generate(context.Background(), generateRequest("hello"))
	if !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("Generate error = %v", err)
	}
	if len(trace.Attempts) != 1 ||
		trace.Attempts[0].Target != first ||
		trace.Attempts[0].Outcome != AttemptOutcomeFailed {
		t.Fatalf("terminal trace = %+v", trace)
	}
}

func TestGenerateRejectsRepeatedFallbackTarget(t *testing.T) {
	first := generateModel("first")
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"first": {reject: inference.UnsupportedFeature},
	})
	router := newGenerateRouter(t, runtime, first, generateFallbackFunc(func(
		context.Context,
		inference.GenerateRequest,
		Attempt,
	) (inference.ModelRef, bool, error) {
		return first, true, nil
	}))
	_, trace, err := router.Generate(context.Background(), generateRequest("hello"))
	if !IsKind(err, FallbackContractViolation) {
		t.Fatalf("Generate error = %v, want fallback contract violation", err)
	}
	if len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %+v", trace.Attempts)
	}
}

func TestGenerateRejectsSelectorTargetWithoutGenerateOperation(t *testing.T) {
	target := generateModel("embed-only")
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"embed-only": {nonGenerate: true},
	})
	router := newGenerateRouter(t, runtime, target, nil)

	_, _, err := router.ExplainGenerate(context.Background(), generateRequest("hello"))
	if !IsKind(err, SelectorContractViolation) {
		t.Fatalf("ExplainGenerate error = %v, want SelectorContractViolation", err)
	}
}

func TestGenerateRejectsFallbackTargetWithoutGenerateOperation(t *testing.T) {
	first := generateModel("first")
	fallback := generateModel("embed-only")
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"first":      {reject: inference.UnsupportedFeature},
		"embed-only": {nonGenerate: true},
	})
	router := newGenerateRouter(t, runtime, first, generateFallbackFunc(func(
		context.Context,
		inference.GenerateRequest,
		Attempt,
	) (inference.ModelRef, bool, error) {
		return fallback, true, nil
	}))

	_, _, err := router.Generate(context.Background(), generateRequest("hello"))
	if !IsKind(err, FallbackContractViolation) {
		t.Fatalf("Generate error = %v, want FallbackContractViolation", err)
	}
}

func TestGenerateRejectsInvalidFallbackTargetAndLimit(t *testing.T) {
	t.Run("invalid target", func(t *testing.T) {
		first := generateModel("first")
		runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
			"first": {reject: inference.UnsupportedFeature},
		})
		router := newGenerateRouter(t, runtime, first, generateFallbackFunc(func(
			context.Context,
			inference.GenerateRequest,
			Attempt,
		) (inference.ModelRef, bool, error) {
			return inference.ModelRef{}, true, nil
		}))
		_, trace, err := router.Generate(context.Background(), generateRequest("hello"))
		if !IsKind(err, FallbackContractViolation) || len(trace.Attempts) != 1 {
			t.Fatalf("Generate error/trace = %v/%+v", err, trace)
		}
	})

	t.Run("target limit", func(t *testing.T) {
		behaviors := make(map[string]generateRouteBehavior, maxGenerateTargets+1)
		targets := make([]inference.ModelRef, maxGenerateTargets+1)
		for index := range targets {
			targets[index] = generateModel(string(rune('a' + index)))
			behaviors[targets[index].ID.Name] = generateRouteBehavior{
				reject: inference.UnsupportedFeature,
			}
		}
		runtime := newGenerateRouteRuntime(t, behaviors)
		next := 1
		router := newGenerateRouter(t, runtime, targets[0], generateFallbackFunc(func(
			context.Context,
			inference.GenerateRequest,
			Attempt,
		) (inference.ModelRef, bool, error) {
			target := targets[next]
			next++
			return target, true, nil
		}))
		_, trace, err := router.Generate(context.Background(), generateRequest("hello"))
		if !IsKind(err, FallbackLimitExceeded) ||
			len(trace.Attempts) != maxGenerateTargets {
			t.Fatalf("Generate error/attempts = %v/%+v", err, trace.Attempts)
		}
	})
}

func TestGenerateFallbackEligibilityIsTransportSafe(t *testing.T) {
	eligible := []inference.ErrorKind{
		inference.UnsupportedOperation,
		inference.UnsupportedFeature,
		inference.InvalidExtension,
	}
	for _, kind := range eligible {
		if !generateFallbackEligible(Attempt{
			Outcome: AttemptOutcomeFailed, ErrorKind: kind,
		}) {
			t.Fatalf("%s is not fallback eligible", kind)
		}
	}
	prohibited := []inference.ErrorKind{
		inference.InvalidRequest,
		inference.PolicyDenied,
		inference.OperationInterrupted,
		inference.CompilerContractViolation,
		inference.ProviderFailure,
		inference.InvalidProviderResponse,
		inference.UnknownProvider,
		inference.UnknownModel,
		inference.UnknownProfile,
	}
	for _, kind := range prohibited {
		if generateFallbackEligible(Attempt{
			Outcome: AttemptOutcomeFailed, ErrorKind: kind,
		}) {
			t.Fatalf("%s is fallback eligible", kind)
		}
	}
	if generateFallbackEligible(Attempt{
		Outcome:          AttemptOutcomeFailed,
		ErrorKind:        inference.UnsupportedFeature,
		ObservableOutput: true,
	}) {
		t.Fatal("attempt with observable output is fallback eligible")
	}
}

func TestGenerateDoesNotFallbackAfterProviderFailure(t *testing.T) {
	first := generateModel("first")
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"first": {transportErr: errors.New("remote failed")},
	})
	fallbackCalls := 0
	router := newGenerateRouter(t, runtime, first, generateFallbackFunc(func(
		context.Context,
		inference.GenerateRequest,
		Attempt,
	) (inference.ModelRef, bool, error) {
		fallbackCalls++
		return generateModel("second"), true, nil
	}))
	_, trace, err := router.Generate(context.Background(), generateRequest("hello"))
	if !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("Generate error = %v, want provider failure", err)
	}
	if fallbackCalls != 0 || len(trace.Attempts) != 2 {
		t.Fatalf("fallback calls/trace = %d/%+v", fallbackCalls, trace.Attempts)
	}
}

func TestGenerateStreamFallsBackBeforeOpen(t *testing.T) {
	first := generateModel("unary-native-stream-rejected")
	second := generateModel("stream")
	firstStreamOpens := 0
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"unary-native-stream-rejected": {
			streamReject: inference.UnsupportedFeature,
			streamOpens:  &firstStreamOpens,
		},
		"stream": {streamEvents: []streamRaw{{finish: true}}},
	})
	router := newGenerateRouter(t, runtime, first, generateFallbackFunc(func(
		_ context.Context,
		_ inference.GenerateRequest,
		attempt Attempt,
	) (inference.ModelRef, bool, error) {
		if attempt.Phase != AttemptPhasePreflight ||
			attempt.ErrorKind != inference.UnsupportedFeature {
			t.Fatalf("fallback attempt = %+v", attempt)
		}
		return second, true, nil
	}))
	if _, _, err := router.Generate(context.Background(), generateRequest("hello")); err != nil {
		t.Fatalf("unary Generate: %v", err)
	}
	stream, trace, err := router.GenerateStream(context.Background(), generateRequest("hello"))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if firstStreamOpens != 0 {
		t.Fatalf("rejected stream transport calls = %d, want 0", firstStreamOpens)
	}
	if len(trace.Attempts) != 3 ||
		trace.Attempts[2].Target != second ||
		trace.Attempts[2].Outcome != AttemptOutcomeOpened {
		t.Fatalf("stream trace = %+v", trace.Attempts)
	}
}

func TestGenerateStreamDoesNotFallbackAfterReturn(t *testing.T) {
	first := generateModel("stream")
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"stream": {streamEvents: []streamRaw{{err: errors.New("late failure")}}},
	})
	fallbackCalls := 0
	router := newGenerateRouter(t, runtime, first, generateFallbackFunc(func(
		context.Context,
		inference.GenerateRequest,
		Attempt,
	) (inference.ModelRef, bool, error) {
		fallbackCalls++
		return generateModel("other"), true, nil
	}))
	stream, trace, err := router.GenerateStream(context.Background(), generateRequest("hello"))
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Next(context.Background()); err == nil {
		t.Fatal("Next succeeded, want late stream failure")
	}
	if fallbackCalls != 0 ||
		len(trace.Attempts) != 2 ||
		trace.Attempts[1].Outcome != AttemptOutcomeOpened ||
		trace.Attempts[1].ObservableOutput {
		t.Fatalf("fallback calls/trace = %d/%+v", fallbackCalls, trace.Attempts)
	}
}

func TestGenerateStreamMarksOpenedAttemptObservableAfterSuccessfulNext(t *testing.T) {
	first := generateModel("stream")
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"stream": {streamEvents: []streamRaw{{}, {finish: true}}},
	})
	fallbackCalls := 0
	router := newGenerateRouter(t, runtime, first, generateFallbackFunc(func(
		context.Context,
		inference.GenerateRequest,
		Attempt,
	) (inference.ModelRef, bool, error) {
		fallbackCalls++
		return generateModel("other"), true, nil
	}))
	stream, trace, err := router.GenerateStream(context.Background(), generateRequest("hello"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if fallbackCalls != 0 {
		t.Fatalf("fallback calls = %d, want 0", fallbackCalls)
	}
	if len(trace.Attempts) != 2 || !trace.Attempts[1].ObservableOutput {
		t.Fatalf("trace after successful Next = %+v", trace.Attempts)
	}
}

type generateRouteBehavior struct {
	reject       inference.ErrorKind
	streamReject inference.ErrorKind
	transportErr error
	unaryOnly    bool
	nonGenerate  bool
	streamEvents []streamRaw
	streamOpens  *int
}

type streamRaw struct {
	finish bool
	err    error
}

type routeProviderStream struct {
	events []streamRaw
	index  int
}

func (s *routeProviderStream) Next(context.Context) (streamRaw, error) {
	if s.index == len(s.events) {
		return streamRaw{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	if event.err != nil {
		return streamRaw{}, event.err
	}
	return event, nil
}

func (*routeProviderStream) Close() error { return nil }

func newGenerateRouteRuntime(
	t *testing.T,
	behaviors map[string]generateRouteBehavior,
) *inference.Runtime {
	t.Helper()
	models := make([]inference.ModelImplementation, 0, len(behaviors))
	for name, behavior := range behaviors {
		behavior := behavior
		if behavior.nonGenerate {
			embed, err := inference.BindEmbed(
				func(
					context.Context,
					inference.ModelRef,
					inference.EmbedRequest,
				) (inference.Compiled[string], error) {
					return inference.Compiled[string]{}, nil
				},
				func(context.Context, string) (string, error) { return "", nil },
				func(context.Context, string) (inference.EmbedResponse, error) {
					return inference.EmbedResponse{}, nil
				},
			)
			if err != nil {
				t.Fatalf("bind embed operation (%s): %v", name, err)
			}
			models = append(models, inference.ModelImplementation{
				Descriptor: inference.ModelDescriptor{
					ID: inference.ModelID{Provider: "fake", Name: name},
				},
				Openers: inference.Openers{
					Embed: func(context.Context, inference.ModelRef) (inference.EmbedDriver, error) {
						return embed, nil
					},
				},
			})
			continue
		}
		compile := func(
			_ context.Context,
			_ inference.ModelRef,
			request inference.GenerateRequest,
			shape inference.GenerateExecutionShape,
		) (inference.Compiled[string], error) {
			active := request.ActiveFieldsFor(shape)
			rejection := behavior.reject
			if shape == inference.GenerateExecutionStream &&
				behavior.streamReject != "" {
				rejection = behavior.streamReject
			}
			if rejection != "" {
				field := active[0]
				if shape == inference.GenerateExecutionStream &&
					behavior.streamReject != "" {
					field = shape.Field()
				}
				return inference.Compiled[string]{Report: inference.CompileReport{
						Operation: inference.OperationGenerate,
						Decisions: []inference.Decision{{
							Field: field, Disposition: inference.Rejected, Reason: "unsupported",
						}},
					}}, inference.NewError(
						rejection,
						inference.OperationGenerate,
						field,
						errors.New("unsupported"),
					)
			}
			decisions := make([]inference.Decision, len(active))
			for index, field := range active {
				decisions[index] = inference.Decision{Field: field, Disposition: inference.Native}
			}
			return inference.Compiled[string]{
				Wire: "wire",
				Report: inference.CompileReport{
					Operation: inference.OperationGenerate,
					Decisions: decisions,
				},
			}, nil
		}
		unaryTransport := func(context.Context, string) (string, error) {
			if behavior.transportErr != nil {
				return "", behavior.transportErr
			}
			return "ok", nil
		}
		unaryDecode := func(context.Context, string) (inference.GenerateResponse, error) {
			return validGenerateResponse(), nil
		}
		var operations inference.GenerateOperations
		var err error
		if behavior.unaryOnly {
			operations.Unary, err = inference.BindGenerate(
				compile, unaryTransport, unaryDecode,
			)
		} else {
			operations, err = inference.BindGenerateOperations(
				compile,
				unaryTransport,
				unaryDecode,
				func(context.Context, string) (inference.ProviderStream[streamRaw], error) {
					if behavior.streamOpens != nil {
						*behavior.streamOpens++
					}
					return &routeProviderStream{events: behavior.streamEvents}, nil
				},
				func(_ context.Context, raw streamRaw) (inference.GenerateStreamEvent, error) {
					if raw.finish {
						return inference.GenerateStreamEvent{FinishReason: inference.FinishCompleted}, nil
					}
					return inference.GenerateStreamEvent{}, nil
				},
			)
		}
		if err != nil {
			t.Fatalf("bind generate operations (%s): %v", name, err)
		}
		models = append(models, inference.ModelImplementation{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: "fake", Name: name},
			},
			Openers: inference.Openers{
				Generate: func(context.Context, inference.ModelRef) (inference.GenerateOperations, error) {
					return operations, nil
				},
			},
		})
	}
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{{
		ID: "fake", Models: models,
	}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

func newGenerateRouter(
	t *testing.T,
	runtime *inference.Runtime,
	first inference.ModelRef,
	fallback GenerateFallbackPolicy,
) *Router {
	t.Helper()
	router, err := New(runtime, Selectors{
		Generate: generateSelectorFunc(func(
			context.Context,
			inference.GenerateRequest,
		) (Decision, error) {
			return generateDecision(first), nil
		}),
		GenerateFallback: fallback,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return router
}

func generateDecision(model inference.ModelRef) Decision {
	return Decision{
		Operation: inference.OperationGenerate,
		Tier:      "balanced",
		Proposed:  model,
		Selected:  model,
	}
}

func generateModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "fake", Name: name},
		Profile: "",
	}
}

func generateRequest(text string) inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: inference.Content{
					Parts: []inference.Part{inference.TextPart{Text: text}},
				},
				Intent: inference.Intent{
					Text: &inference.TextIntent{},
				},
			},
		},
	}
}

func validGenerateResponse() inference.GenerateResponse {
	return inference.GenerateResponse{
		Message: inference.Message{
			Role: inference.RoleAssistant,
			Content: inference.Content{
				Parts: []inference.Part{inference.TextPart{Text: "ok"}},
			},
		},
		FinishReason: inference.FinishCompleted,
	}
}
