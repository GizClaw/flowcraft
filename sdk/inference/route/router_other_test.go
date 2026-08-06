package route

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

type embedSelectorFunc func(context.Context, inference.EmbedRequest) (Decision, error)

func (f embedSelectorFunc) SelectEmbed(
	ctx context.Context,
	request inference.EmbedRequest,
) (Decision, error) {
	return f(ctx, request)
}

type transcriptionSelectorFunc func(
	context.Context,
	inference.TranscriptionRequest,
) (Decision, error)

func (f transcriptionSelectorFunc) SelectTranscription(
	ctx context.Context,
	request inference.TranscriptionRequest,
) (Decision, error) {
	return f(ctx, request)
}

type realtimeSelectorFunc func(context.Context, inference.RealtimeConfig) (Decision, error)

func (f realtimeSelectorFunc) SelectRealtime(
	ctx context.Context,
	config inference.RealtimeConfig,
) (Decision, error) {
	return f(ctx, config)
}

func TestRouterExposesSelectorsForCurrentOperations(t *testing.T) {
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"model": {},
	})
	invalid := Decision{Operation: inference.OperationGenerate}

	t.Run("embed", func(t *testing.T) {
		router, err := New(runtime, Selectors{Embed: embedSelectorFunc(func(
			context.Context,
			inference.EmbedRequest,
		) (Decision, error) {
			return invalid, nil
		})})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, _, err = router.ExplainEmbed(context.Background(), inference.EmbedRequest{
			Items: []inference.EmbedItem{{Content: message.Content{
				Parts: []message.Part{message.TextPart{Text: "hello"}},
			}}},
		})
		assertSelectorContractViolation(t, err)
	})

	t.Run("transcription", func(t *testing.T) {
		audio, err := media.NewAudioBytes([]byte("audio"), "audio/wav")
		if err != nil {
			t.Fatalf("NewAudioBytes: %v", err)
		}
		router, err := New(runtime, Selectors{
			Transcription: transcriptionSelectorFunc(func(
				context.Context,
				inference.TranscriptionRequest,
			) (Decision, error) {
				return invalid, nil
			}),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, _, err = router.ExplainTranscription(
			context.Background(),
			inference.TranscriptionRequest{Audio: audio},
		)
		assertSelectorContractViolation(t, err)
	})

	t.Run("realtime", func(t *testing.T) {
		router, err := New(runtime, Selectors{Realtime: realtimeSelectorFunc(func(
			context.Context,
			inference.RealtimeConfig,
		) (Decision, error) {
			return invalid, nil
		})})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, _, err = router.ExplainRealtime(
			context.Background(),
			inference.RealtimeConfig{Modalities: []inference.Modality{
				inference.ModalityText,
			}},
		)
		assertSelectorContractViolation(t, err)
	})
}

func assertSelectorContractViolation(t *testing.T, err error) {
	t.Helper()
	if !IsKind(err, SelectorContractViolation) {
		t.Fatalf("error = %v, want SelectorContractViolation", err)
	}
}

type embedFallbackFunc func(
	context.Context,
	inference.EmbedRequest,
	Attempt,
) (inference.ModelRef, bool, error)

func (f embedFallbackFunc) NextEmbed(
	ctx context.Context,
	request inference.EmbedRequest,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return f(ctx, request, attempt)
}

type transcriptionFallbackFunc func(
	context.Context,
	inference.TranscriptionRequest,
	Attempt,
) (inference.ModelRef, bool, error)

func (f transcriptionFallbackFunc) NextTranscription(
	ctx context.Context,
	request inference.TranscriptionRequest,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return f(ctx, request, attempt)
}

type transcriptionSessionFallbackFunc func(
	context.Context,
	inference.TranscriptionSessionConfig,
	Attempt,
) (inference.ModelRef, bool, error)

func (f transcriptionSessionFallbackFunc) NextTranscriptionSession(
	ctx context.Context,
	config inference.TranscriptionSessionConfig,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return f(ctx, config, attempt)
}

type realtimeFallbackFunc func(
	context.Context,
	inference.RealtimeConfig,
	Attempt,
) (inference.ModelRef, bool, error)

func (f realtimeFallbackFunc) NextRealtime(
	ctx context.Context,
	config inference.RealtimeConfig,
	attempt Attempt,
) (inference.ModelRef, bool, error) {
	return f(ctx, config, attempt)
}

func TestNewRejectsFallbackPolicyWithoutSelector(t *testing.T) {
	runtime := newEmbedRouteRuntime(t, map[string]embedRouteBehavior{"model": {}})
	_, err := New(runtime, Selectors{
		EmbedFallback: embedFallbackFunc(func(
			context.Context,
			inference.EmbedRequest,
			Attempt,
		) (inference.ModelRef, bool, error) {
			return inference.ModelRef{}, false, nil
		}),
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("New error = %v, want validation failure", err)
	}
}

func TestEmbedFallsBackOnCompilerRejection(t *testing.T) {
	first := embedModel("first")
	second := embedModel("second")
	runtime := newEmbedRouteRuntime(t, map[string]embedRouteBehavior{
		"first":  {reject: inference.UnsupportedFeature},
		"second": {},
	})
	fallbackCalls := 0
	router, err := New(runtime, Selectors{
		Embed: embedSelectorFunc(func(
			_ context.Context,
			request inference.EmbedRequest,
		) (Decision, error) {
			request.Items[0].Content.Parts[0] = message.TextPart{Text: "selector mutation"}
			return embedDecision(first), nil
		}),
		EmbedFallback: embedFallbackFunc(func(
			_ context.Context,
			request inference.EmbedRequest,
			attempt Attempt,
		) (inference.ModelRef, bool, error) {
			fallbackCalls++
			request.Items[0].Content.Parts[0] = message.TextPart{Text: "fallback mutation"}
			if attempt.Target != first ||
				attempt.Phase != AttemptPhaseExecute ||
				attempt.ErrorKind != inference.UnsupportedFeature {
				t.Fatalf("fallback attempt = %+v", attempt)
			}
			return second, true, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := embedRequest("original")
	response, trace, err := router.Embed(context.Background(), request)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if fallbackCalls != 1 || response.Metadata.Model != second.ID {
		t.Fatalf("fallback calls/model = %d/%+v", fallbackCalls, response.Metadata.Model)
	}
	if trace.Executed != second || len(trace.Fallbacks) != 1 {
		t.Fatalf("trace = %+v", trace)
	}
	if len(trace.Attempts) != 2 ||
		trace.Attempts[0].Outcome != AttemptOutcomeFailed ||
		trace.Attempts[1].Outcome != AttemptOutcomeSucceeded {
		t.Fatalf("trace attempts = %+v", trace.Attempts)
	}
	if got := request.Items[0].Content.Parts[0].(message.TextPart).Text; got != "original" {
		t.Fatalf("caller request mutated to %q", got)
	}
}

func TestEmbedFallbackStopReturnsOriginalError(t *testing.T) {
	first := embedModel("first")
	runtime := newEmbedRouteRuntime(t, map[string]embedRouteBehavior{
		"first": {reject: inference.UnsupportedFeature},
	})
	router, err := New(runtime, Selectors{
		Embed: embedSelectorFunc(func(
			context.Context,
			inference.EmbedRequest,
		) (Decision, error) {
			return embedDecision(first), nil
		}),
		EmbedFallback: embedFallbackFunc(func(
			context.Context,
			inference.EmbedRequest,
			Attempt,
		) (inference.ModelRef, bool, error) {
			return inference.ModelRef{}, false, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, trace, err := router.Embed(context.Background(), embedRequest("hello"))
	if !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("Embed error = %v, want unsupported feature", err)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Target != first {
		t.Fatalf("terminal trace = %+v", trace)
	}
}

func TestEmbedRejectsFallbackTargetWithoutEmbedOperation(t *testing.T) {
	first := embedModel("first")
	fallback := embedModel("generate-only")
	runtime := newEmbedRouteRuntime(t, map[string]embedRouteBehavior{
		"first":         {reject: inference.UnsupportedFeature},
		"generate-only": {generateOnly: true},
	})
	router, err := New(runtime, Selectors{
		Embed: embedSelectorFunc(func(
			context.Context,
			inference.EmbedRequest,
		) (Decision, error) {
			return embedDecision(first), nil
		}),
		EmbedFallback: embedFallbackFunc(func(
			context.Context,
			inference.EmbedRequest,
			Attempt,
		) (inference.ModelRef, bool, error) {
			return fallback, true, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = router.Embed(context.Background(), embedRequest("hello"))
	if !IsKind(err, FallbackContractViolation) {
		t.Fatalf("Embed error = %v, want FallbackContractViolation", err)
	}
}

func TestTranscribeFallsBackOnCompilerRejection(t *testing.T) {
	first := embedModel("first")
	second := embedModel("second")
	runtime := newTranscriptionRouteRuntime(t, map[string]embedRouteBehavior{
		"first":  {reject: inference.UnsupportedFeature},
		"second": {},
	})
	router, err := New(runtime, Selectors{
		Transcription: transcriptionSelectorFunc(func(
			context.Context,
			inference.TranscriptionRequest,
		) (Decision, error) {
			return transcriptionDecision(first), nil
		}),
		TranscriptionFallback: transcriptionFallbackFunc(func(
			_ context.Context,
			_ inference.TranscriptionRequest,
			attempt Attempt,
		) (inference.ModelRef, bool, error) {
			if attempt.Target != first ||
				attempt.Phase != AttemptPhaseExecute ||
				attempt.ErrorKind != inference.UnsupportedFeature {
				t.Fatalf("fallback attempt = %+v", attempt)
			}
			return second, true, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, trace, err := router.Transcribe(
		context.Background(),
		transcriptionRequest(t),
	)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if response.Text != "ok" || response.Metadata.Model != second.ID {
		t.Fatalf("response = %+v", response)
	}
	if trace.Executed != second || len(trace.Fallbacks) != 1 ||
		len(trace.Attempts) != 2 {
		t.Fatalf("trace = %+v", trace)
	}
}

func TestOpenTranscriptionFallsBackBeforeOpen(t *testing.T) {
	first := embedModel("first")
	second := embedModel("second")
	firstOpens := 0
	runtime := newTranscriptionSessionRouteRuntime(t, map[string]transcriptionSessionRouteBehavior{
		"first":  {reject: inference.UnsupportedFeature, opens: &firstOpens},
		"second": {},
	})
	router, err := New(runtime, Selectors{
		TranscriptionSession: transcriptionSessionSelectorFunc(func(
			context.Context,
			inference.TranscriptionSessionConfig,
		) (Decision, error) {
			return transcriptionSessionDecision(first), nil
		}),
		TranscriptionSessionFallback: transcriptionSessionFallbackFunc(func(
			_ context.Context,
			_ inference.TranscriptionSessionConfig,
			attempt Attempt,
		) (inference.ModelRef, bool, error) {
			if attempt.Target != first ||
				attempt.Phase != AttemptPhaseOpen ||
				attempt.ErrorKind != inference.UnsupportedFeature {
				t.Fatalf("fallback attempt = %+v", attempt)
			}
			return second, true, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, trace, err := router.OpenTranscription(
		context.Background(),
		transcriptionSessionConfig(),
	)
	if err != nil {
		t.Fatalf("OpenTranscription: %v", err)
	}
	defer func() { _ = session.Close() }()
	if firstOpens != 0 {
		t.Fatalf("rejected session transport opens = %d, want 0", firstOpens)
	}
	if trace.Executed != second ||
		len(trace.Attempts) != 2 ||
		trace.Attempts[0].Phase != AttemptPhaseOpen ||
		trace.Attempts[0].Outcome != AttemptOutcomeFailed ||
		trace.Attempts[1].Outcome != AttemptOutcomeOpened {
		t.Fatalf("session trace = %+v", trace)
	}
}

func TestOpenRealtimeFallsBackBeforeOpen(t *testing.T) {
	first := embedModel("first")
	second := embedModel("second")
	firstOpens := 0
	runtime := newRealtimeRouteRuntime(t, map[string]realtimeRouteBehavior{
		"first":  {reject: inference.UnsupportedFeature, opens: &firstOpens},
		"second": {},
	})
	router, err := New(runtime, Selectors{
		Realtime: realtimeSelectorFunc(func(
			context.Context,
			inference.RealtimeConfig,
		) (Decision, error) {
			return realtimeDecision(first), nil
		}),
		RealtimeFallback: realtimeFallbackFunc(func(
			_ context.Context,
			_ inference.RealtimeConfig,
			attempt Attempt,
		) (inference.ModelRef, bool, error) {
			if attempt.Target != first ||
				attempt.Phase != AttemptPhaseOpen ||
				attempt.ErrorKind != inference.UnsupportedFeature {
				t.Fatalf("fallback attempt = %+v", attempt)
			}
			return second, true, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, trace, err := router.OpenRealtime(
		context.Background(),
		inference.RealtimeConfig{Modalities: []inference.Modality{inference.ModalityText}},
	)
	if err != nil {
		t.Fatalf("OpenRealtime: %v", err)
	}
	defer func() { _ = session.Close() }()
	if firstOpens != 0 {
		t.Fatalf("rejected realtime transport opens = %d, want 0", firstOpens)
	}
	if trace.Executed != second ||
		len(trace.Attempts) != 2 ||
		trace.Attempts[1].Outcome != AttemptOutcomeOpened {
		t.Fatalf("realtime trace = %+v", trace)
	}
}

type embedRouteBehavior struct {
	reject       inference.ErrorKind
	generateOnly bool
}

func newEmbedRouteRuntime(
	t *testing.T,
	behaviors map[string]embedRouteBehavior,
) *inference.Runtime {
	t.Helper()
	models := make([]inference.ModelImplementation, 0, len(behaviors))
	for name, behavior := range behaviors {
		behavior := behavior
		if behavior.generateOnly {
			var operations inference.GenerateOperations
			unary, err := inference.BindGenerate(
				func(
					_ context.Context,
					_ inference.ModelRef,
					request inference.GenerateRequest,
					shape inference.GenerateExecutionShape,
				) (inference.Compiled[string], error) {
					active := request.ActiveFieldsFor(shape)
					decisions := make([]inference.Decision, len(active))
					for index, field := range active {
						decisions[index] = inference.Decision{
							Field: field, Disposition: inference.Native,
						}
					}
					return inference.Compiled[string]{
						Wire: "wire",
						Report: inference.CompileReport{
							Operation: inference.OperationGenerate, Decisions: decisions,
						},
					}, nil
				},
				func(context.Context, string) (string, error) { return "ok", nil },
				func(context.Context, string) (inference.GenerateResponse, error) {
					return validGenerateResponse(), nil
				},
			)
			if err != nil {
				t.Fatalf("bind generate operation (%s): %v", name, err)
			}
			operations.Unary = unary
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
			continue
		}
		driver, err := inference.BindEmbed(
			func(
				_ context.Context,
				_ inference.ModelRef,
				request inference.EmbedRequest,
			) (inference.Compiled[string], error) {
				active := request.ActiveFields()
				if behavior.reject != "" {
					return inference.Compiled[string]{
							Report: inference.CompileReport{
								Operation: inference.OperationEmbed,
								Decisions: []inference.Decision{{
									Field:       active[0],
									Disposition: inference.Rejected,
									Reason:      "unsupported",
								}},
							},
						}, inference.NewError(
							behavior.reject,
							inference.OperationEmbed,
							active[0],
							errors.New("unsupported"),
						)
				}
				decisions := make([]inference.Decision, len(active))
				for index, field := range active {
					decisions[index] = inference.Decision{
						Field: field, Disposition: inference.Native,
					}
				}
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inference.CompileReport{
						Operation: inference.OperationEmbed, Decisions: decisions,
					},
				}, nil
			},
			func(context.Context, string) (string, error) { return "ok", nil },
			func(context.Context, string) (inference.EmbedResponse, error) {
				return inference.EmbedResponse{
					Embeddings: []inference.Embedding{{Vector: []float32{1, 0}}},
					Usage:      inference.EmbedUsage{ItemCount: 1},
				}, nil
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
					return driver, nil
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

func newTranscriptionRouteRuntime(
	t *testing.T,
	behaviors map[string]embedRouteBehavior,
) *inference.Runtime {
	t.Helper()
	models := make([]inference.ModelImplementation, 0, len(behaviors))
	for name, behavior := range behaviors {
		behavior := behavior
		driver, err := inference.BindTranscription(
			func(
				_ context.Context,
				_ inference.ModelRef,
				request inference.TranscriptionRequest,
			) (inference.Compiled[string], error) {
				active := request.ActiveFields()
				if behavior.reject != "" {
					return inference.Compiled[string]{
							Report: inference.CompileReport{
								Operation: inference.OperationTranscription,
								Decisions: []inference.Decision{{
									Field:       active[0],
									Disposition: inference.Rejected,
									Reason:      "unsupported",
								}},
							},
						}, inference.NewError(
							behavior.reject,
							inference.OperationTranscription,
							active[0],
							errors.New("unsupported"),
						)
				}
				decisions := make([]inference.Decision, len(active))
				for index, field := range active {
					decisions[index] = inference.Decision{
						Field: field, Disposition: inference.Native,
					}
				}
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inference.CompileReport{
						Operation: inference.OperationTranscription, Decisions: decisions,
					},
				}, nil
			},
			func(context.Context, string) (string, error) { return "ok", nil },
			func(context.Context, string) (inference.TranscriptionResponse, error) {
				return inference.TranscriptionResponse{Text: "ok"}, nil
			},
		)
		if err != nil {
			t.Fatalf("bind transcription operation (%s): %v", name, err)
		}
		models = append(models, inference.ModelImplementation{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: "fake", Name: name},
			},
			Openers: inference.Openers{
				Transcription: func(context.Context, inference.ModelRef) (inference.TranscriptionOperations, error) {
					return inference.TranscriptionOperations{Unary: driver}, nil
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

type transcriptionSessionRouteBehavior struct {
	reject inference.ErrorKind
	opens  *int
}

type stubRouteTranscriptionSession struct{}

func (stubRouteTranscriptionSession) SendAudio(context.Context, media.AudioChunk) error {
	return nil
}

func (stubRouteTranscriptionSession) CloseInput(context.Context) error { return nil }

func (stubRouteTranscriptionSession) Next(
	context.Context,
) (inference.TranscriptionEvent, error) {
	return nil, io.EOF
}

func (stubRouteTranscriptionSession) Result() (inference.TranscriptionResponse, error) {
	return inference.TranscriptionResponse{}, io.EOF
}

func (stubRouteTranscriptionSession) Close() error { return nil }

func newTranscriptionSessionRouteRuntime(
	t *testing.T,
	behaviors map[string]transcriptionSessionRouteBehavior,
) *inference.Runtime {
	t.Helper()
	models := make([]inference.ModelImplementation, 0, len(behaviors))
	for name, behavior := range behaviors {
		behavior := behavior
		driver, err := inference.BindTranscriptionSession(
			func(
				_ context.Context,
				_ inference.ModelRef,
				config inference.TranscriptionSessionConfig,
			) (inference.Compiled[string], error) {
				active := config.ActiveFields()
				if behavior.reject != "" {
					return inference.Compiled[string]{
							Report: inference.CompileReport{
								Operation: inference.OperationTranscription,
								Decisions: []inference.Decision{{
									Field:       active[0],
									Disposition: inference.Rejected,
									Reason:      "unsupported",
								}},
							},
						}, inference.NewError(
							behavior.reject,
							inference.OperationTranscription,
							active[0],
							errors.New("unsupported"),
						)
				}
				decisions := make([]inference.Decision, len(active))
				for index, field := range active {
					decisions[index] = inference.Decision{
						Field: field, Disposition: inference.Native,
					}
				}
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inference.CompileReport{
						Operation: inference.OperationTranscription, Decisions: decisions,
					},
				}, nil
			},
			func(context.Context, string) (inference.TranscriptionSession, error) {
				if behavior.opens != nil {
					*behavior.opens++
				}
				return stubRouteTranscriptionSession{}, nil
			},
		)
		if err != nil {
			t.Fatalf("bind transcription session (%s): %v", name, err)
		}
		models = append(models, inference.ModelImplementation{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: "fake", Name: name},
			},
			Openers: inference.Openers{
				Transcription: func(context.Context, inference.ModelRef) (inference.TranscriptionOperations, error) {
					return inference.TranscriptionOperations{Session: driver}, nil
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

type realtimeRouteBehavior struct {
	reject inference.ErrorKind
	opens  *int
}

type stubRouteRealtimeSession struct{}

func (stubRouteRealtimeSession) Send(context.Context, string) error { return nil }

func (stubRouteRealtimeSession) Next(context.Context) (string, error) {
	return "", io.EOF
}

func (stubRouteRealtimeSession) CancelResponse(context.Context) error { return nil }

func (stubRouteRealtimeSession) Close() error { return nil }

func newRealtimeRouteRuntime(
	t *testing.T,
	behaviors map[string]realtimeRouteBehavior,
) *inference.Runtime {
	t.Helper()
	models := make([]inference.ModelImplementation, 0, len(behaviors))
	for name, behavior := range behaviors {
		behavior := behavior
		driver, err := inference.BindRealtime(
			func(
				_ context.Context,
				_ inference.ModelRef,
				config inference.RealtimeConfig,
			) (inference.Compiled[string], error) {
				active := config.ActiveFields()
				if behavior.reject != "" {
					return inference.Compiled[string]{
							Report: inference.CompileReport{
								Operation: inference.OperationRealtime,
								Decisions: []inference.Decision{{
									Field:       active[0],
									Disposition: inference.Rejected,
									Reason:      "unsupported",
								}},
							},
						}, inference.NewError(
							behavior.reject,
							inference.OperationRealtime,
							active[0],
							errors.New("unsupported"),
						)
				}
				decisions := make([]inference.Decision, len(active))
				for index, field := range active {
					decisions[index] = inference.Decision{
						Field: field, Disposition: inference.Native,
					}
				}
				return inference.Compiled[string]{
					Wire: "wire",
					Report: inference.CompileReport{
						Operation: inference.OperationRealtime, Decisions: decisions,
					},
				}, nil
			},
			func(context.Context, string) (inference.ProviderRealtimeSession[string, string], error) {
				if behavior.opens != nil {
					*behavior.opens++
				}
				return stubRouteRealtimeSession{}, nil
			},
			func(
				context.Context,
				inference.ModelRef,
				inference.RealtimeInput,
			) (inference.Compiled[string], error) {
				return inference.Compiled[string]{}, nil
			},
			func(context.Context, string) (inference.RealtimeEvent, error) {
				return inference.RealtimeTextDeltaEvent{Delta: "ok"}, nil
			},
		)
		if err != nil {
			t.Fatalf("bind realtime session (%s): %v", name, err)
		}
		models = append(models, inference.ModelImplementation{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: "fake", Name: name},
			},
			Openers: inference.Openers{
				Realtime: func(context.Context, inference.ModelRef) (inference.RealtimeDriver, error) {
					return driver, nil
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

type transcriptionSessionSelectorFunc func(
	context.Context,
	inference.TranscriptionSessionConfig,
) (Decision, error)

func (f transcriptionSessionSelectorFunc) SelectTranscriptionSession(
	ctx context.Context,
	config inference.TranscriptionSessionConfig,
) (Decision, error) {
	return f(ctx, config)
}

func embedModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID: inference.ModelID{Provider: "fake", Name: name},
	}
}

func embedDecision(model inference.ModelRef) Decision {
	return Decision{
		Operation: inference.OperationEmbed,
		Tier:      "balanced",
		Proposed:  model,
		Selected:  model,
	}
}

func transcriptionDecision(model inference.ModelRef) Decision {
	return Decision{
		Operation: inference.OperationTranscription,
		Tier:      "balanced",
		Proposed:  model,
		Selected:  model,
	}
}

func transcriptionSessionDecision(model inference.ModelRef) Decision {
	return Decision{
		Operation: inference.OperationTranscription,
		Tier:      "balanced",
		Proposed:  model,
		Selected:  model,
	}
}

func realtimeDecision(model inference.ModelRef) Decision {
	return Decision{
		Operation: inference.OperationRealtime,
		Tier:      "balanced",
		Proposed:  model,
		Selected:  model,
	}
}

func embedRequest(text string) inference.EmbedRequest {
	return inference.EmbedRequest{
		Items: []inference.EmbedItem{{Content: message.Content{
			Parts: []message.Part{message.TextPart{Text: text}},
		}}},
	}
}

func transcriptionRequest(t *testing.T) inference.TranscriptionRequest {
	t.Helper()
	audio, err := media.NewAudioBytes([]byte("audio"), "audio/wav")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	return inference.TranscriptionRequest{Audio: audio}
}

func transcriptionSessionConfig() inference.TranscriptionSessionConfig {
	return inference.TranscriptionSessionConfig{
		InputFormat: media.AudioFormat{
			Encoding:     media.AudioEncodingPCM16,
			SampleRateHz: 16000,
			Channels:     1,
		},
	}
}
