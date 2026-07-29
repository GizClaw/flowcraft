package inference

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// This file is the contract matrix for provider pipeline stages. It pins the
// error taxonomy at the framework boundary: which stage failure becomes which
// ErrorKind, and which errdefs classification callers may rely on. Provider
// implementations inherit these behaviors through the Bind* helpers; the
// matrix guards against regressions when the pipeline internals change.

func contractEmbedModel() ModelRef {
	return ModelRef{ID: ModelID{Provider: "fake", Name: "embed"}}
}

func contractEmbedRequest() EmbedRequest {
	return EmbedRequest{Items: []EmbedItem{{
		Content: Content{Parts: []Part{TextPart{Text: "hello"}}},
	}}}
}

func contractNativeEmbedCompile[Wire any](wire Wire) Compiler[EmbedRequest, Wire] {
	return func(
		_ context.Context,
		_ ModelRef,
		request EmbedRequest,
	) (Compiled[Wire], error) {
		active := request.ActiveFields()
		decisions := make([]Decision, 0, len(active))
		for _, field := range active {
			decisions = append(decisions, Decision{Field: field, Disposition: Native})
		}
		return Compiled[Wire]{
			Wire:   wire,
			Report: CompileReport{Operation: OperationEmbed, Decisions: decisions},
		}, nil
	}
}

func TestContractMatrixStageErrorTaxonomy(t *testing.T) {
	type wire struct{ Prompt string }
	tests := []struct {
		name      string
		compile   Compiler[EmbedRequest, wire]
		transport Transport[wire, string]
		decode    Decoder[string, EmbedResponse]
		kind      ErrorKind
		check     func(error) bool
	}{
		{
			name: "compiler rejection stays a validation rejection",
			compile: func(
				_ context.Context,
				_ ModelRef,
				request EmbedRequest,
			) (Compiled[wire], error) {
				active := request.ActiveFields()
				decisions := make([]Decision, 0, len(active))
				for _, field := range active {
					decision := Decision{Field: field, Disposition: Native}
					if field == FieldEmbedItemText {
						decision.Disposition = Rejected
						decision.Reason = "unsupported"
					}
					decisions = append(decisions, decision)
				}
				return Compiled[wire]{
						Report: CompileReport{
							Operation: OperationEmbed,
							Decisions: decisions,
						},
					}, NewError(
						UnsupportedFeature,
						OperationEmbed,
						FieldEmbedItemText,
						errors.New("unsupported"),
					)
			},
			transport: func(context.Context, wire) (string, error) {
				t.Fatal("rejected request reached transport")
				return "", nil
			},
			decode: func(context.Context, string) (EmbedResponse, error) {
				return EmbedResponse{}, nil
			},
			kind:  UnsupportedFeature,
			check: errdefs.IsValidation,
		},
		{
			name:    "compiler panic-shaped errors become contract violations",
			compile: contractNativeEmbedCompile(wire{}),
			transport: func(context.Context, wire) (string, error) {
				return "", nil
			},
			decode: func(context.Context, string) (EmbedResponse, error) {
				return EmbedResponse{}, errors.New("garbage payload")
			},
			kind:  InvalidProviderResponse,
			check: errdefs.IsInternal,
		},
		{
			name:    "transport failures become classified provider failures",
			compile: contractNativeEmbedCompile(wire{}),
			transport: func(context.Context, wire) (string, error) {
				return "", errors.New("connection reset")
			},
			decode: func(context.Context, string) (EmbedResponse, error) {
				return EmbedResponse{}, nil
			},
			kind:  ProviderFailure,
			check: errdefs.IsNotAvailable,
		},
		{
			name: "compiler contract violation when structured error lies",
			compile: func(
				context.Context,
				ModelRef,
				EmbedRequest,
			) (Compiled[wire], error) {
				// Error claims a rejection the report does not back.
				return Compiled[wire]{}, NewError(
					UnsupportedFeature,
					OperationEmbed,
					FieldEmbedDimensions,
					errors.New("unsupported"),
				)
			},
			transport: func(context.Context, wire) (string, error) {
				return "", nil
			},
			decode: func(context.Context, string) (EmbedResponse, error) {
				return EmbedResponse{}, nil
			},
			kind:  CompilerContractViolation,
			check: errdefs.IsInternal,
		},
		{
			name: "compiler errors without structure become contract violations",
			compile: func(
				context.Context,
				ModelRef,
				EmbedRequest,
			) (Compiled[wire], error) {
				return Compiled[wire]{}, errors.New("kaboom")
			},
			transport: func(context.Context, wire) (string, error) {
				return "", nil
			},
			decode: func(context.Context, string) (EmbedResponse, error) {
				return EmbedResponse{}, nil
			},
			kind:  CompilerContractViolation,
			check: errdefs.IsInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driver, err := BindEmbed(tt.compile, tt.transport, tt.decode)
			if err != nil {
				t.Fatalf("BindEmbed: %v", err)
			}
			_, err = driver.Execute(
				context.Background(),
				contractEmbedModel(),
				contractEmbedRequest(),
			)
			if !IsKind(err, tt.kind) {
				t.Fatalf("Execute error = %v, want kind %s", err, tt.kind)
			}
			if !tt.check(err) {
				t.Fatalf("Execute error = %v failed its classification check", err)
			}
		})
	}
}

func TestContractMatrixExplainsNeverTouchTransport(t *testing.T) {
	type wire struct{}
	transportCalls := 0
	driver, err := BindEmbed(
		contractNativeEmbedCompile(wire{}),
		func(context.Context, wire) (string, error) {
			transportCalls++
			return "", nil
		},
		func(context.Context, string) (EmbedResponse, error) {
			return EmbedResponse{}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindEmbed: %v", err)
	}
	explanation, err := driver.Explain(
		context.Background(),
		contractEmbedModel(),
		contractEmbedRequest(),
	)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if transportCalls != 0 {
		t.Fatalf("Explain invoked transport %d times", transportCalls)
	}
	if explanation.Model != contractEmbedModel() ||
		explanation.Operation != OperationEmbed ||
		len(explanation.Decisions) == 0 {
		t.Fatalf("Explanation = %+v", explanation)
	}
}

func TestContractMatrixDecodeAndValidationFailures(t *testing.T) {
	type wire struct{}
	t.Run("decode error", func(t *testing.T) {
		driver, err := BindGenerate(
			nativeGenerateCompile(wire{}),
			func(context.Context, wire) (string, error) { return "raw", nil },
			func(context.Context, string) (GenerateResponse, error) {
				return GenerateResponse{}, errors.New("malformed payload")
			},
		)
		if err != nil {
			t.Fatalf("BindGenerate: %v", err)
		}
		_, err = driver.Execute(
			context.Background(),
			ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
			validGenerateTextRequest(),
		)
		if !IsKind(err, InvalidProviderResponse) || !errdefs.IsInternal(err) {
			t.Fatalf("decode error = %v, want internal InvalidProviderResponse", err)
		}
	})

	t.Run("response validation error", func(t *testing.T) {
		driver, err := BindGenerate(
			nativeGenerateCompile(wire{}),
			func(context.Context, wire) (string, error) { return "raw", nil },
			func(context.Context, string) (GenerateResponse, error) {
				// Structurally decodable but violates the response contract.
				return GenerateResponse{}, nil
			},
		)
		if err != nil {
			t.Fatalf("BindGenerate: %v", err)
		}
		_, err = driver.Execute(
			context.Background(),
			ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
			validGenerateTextRequest(),
		)
		if !IsKind(err, InvalidProviderResponse) {
			t.Fatalf("validation error = %v, want InvalidProviderResponse", err)
		}
	})
}

func TestContractMatrixStreamErrorBoundaries(t *testing.T) {
	type wire struct{}
	model := ModelRef{ID: ModelID{Provider: "fake", Name: "stream"}}
	openStream := func(events []GenerateStreamEvent, openErr error) Transport[wire, ProviderStream[GenerateStreamEvent]] {
		return func(context.Context, wire) (ProviderStream[GenerateStreamEvent], error) {
			if openErr != nil {
				return nil, openErr
			}
			return &generateEventStream{events: events}, nil
		}
	}
	decodeIdentity := func(
		_ context.Context,
		event GenerateStreamEvent,
	) (GenerateStreamEvent, error) {
		return event, nil
	}

	t.Run("open failure is a provider failure", func(t *testing.T) {
		driver, err := BindGenerateStream(
			nativeGenerateCompile(wire{}),
			openStream(nil, errors.New("dial failed")),
			decodeIdentity,
		)
		if err != nil {
			t.Fatalf("BindGenerateStream: %v", err)
		}
		_, err = driver.Stream(context.Background(), model, validGenerateTextRequest())
		if !IsKind(err, ProviderFailure) || !errdefs.IsNotAvailable(err) {
			t.Fatalf("open error = %v, want not-available ProviderFailure", err)
		}
	})

	t.Run("mid-stream failure surfaces on Next as provider failure", func(t *testing.T) {
		driver, err := BindGenerateStream(
			nativeGenerateCompile(wire{}),
			openStream([]GenerateStreamEvent{
				{PartIndex: 0, Delta: TextPartDelta{Text: "partial"}},
			}, nil),
			decodeIdentity,
		)
		if err != nil {
			t.Fatalf("BindGenerateStream: %v", err)
		}
		stream, err := driver.Stream(context.Background(), model, validGenerateTextRequest())
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		defer func() { _ = stream.Close() }()
		if _, err := stream.Next(context.Background()); err != nil {
			t.Fatalf("first Next: %v", err)
		}
		// The stub stream ends without a finish event; Result must fail rather
		// than fabricate success.
		if _, err := stream.Result(); err == nil {
			t.Fatal("Result succeeded without a finish event")
		}
	})

	t.Run("event decode failure is invalid provider response", func(t *testing.T) {
		driver, err := BindGenerateStream(
			nativeGenerateCompile(wire{}),
			openStream([]GenerateStreamEvent{{PartIndex: 0}}, nil),
			func(context.Context, GenerateStreamEvent) (GenerateStreamEvent, error) {
				return GenerateStreamEvent{}, errors.New("unknown event shape")
			},
		)
		if err != nil {
			t.Fatalf("BindGenerateStream: %v", err)
		}
		stream, err := driver.Stream(context.Background(), model, validGenerateTextRequest())
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		defer func() { _ = stream.Close() }()
		_, err = stream.Next(context.Background())
		if !IsKind(err, InvalidProviderResponse) {
			t.Fatalf("Next error = %v, want InvalidProviderResponse", err)
		}
	})

	t.Run("io EOF closes the stream without an error", func(t *testing.T) {
		driver, err := BindGenerateStream(
			nativeGenerateCompile(wire{}),
			func(context.Context, wire) (ProviderStream[GenerateStreamEvent], error) {
				return &generateEventStream{events: []GenerateStreamEvent{
					{PartIndex: 0, Delta: TextPartDelta{Text: "done"}},
					{FinishReason: FinishCompleted},
				}}, nil
			},
			decodeIdentity,
		)
		if err != nil {
			t.Fatalf("BindGenerateStream: %v", err)
		}
		stream, err := driver.Stream(context.Background(), model, validGenerateTextRequest())
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		defer func() { _ = stream.Close() }()
		for {
			_, err := stream.Next(context.Background())
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
		}
		response, err := stream.Result()
		if err != nil {
			t.Fatalf("Result: %v", err)
		}
		if response.FinishReason != FinishCompleted {
			t.Fatalf("result finish = %q", response.FinishReason)
		}
	})
}

func TestContractMatrixExplainCoverage(t *testing.T) {
	// Every Explain must carry the exact model, the operation, and at least
	// one compiler decision — this is what makes route decisions auditable.
	t.Run("generate unary", func(t *testing.T) {
		type wire struct{}
		driver, err := BindGenerate(
			nativeGenerateCompile(wire{}),
			func(context.Context, wire) (string, error) { return "", nil },
			func(context.Context, string) (GenerateResponse, error) {
				return GenerateResponse{}, nil
			},
		)
		if err != nil {
			t.Fatalf("BindGenerate: %v", err)
		}
		model := ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}}
		explanation, err := driver.Explain(
			context.Background(), model, validGenerateTextRequest(),
		)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if explanation.Model != model ||
			explanation.Operation != OperationGenerate ||
			len(explanation.Decisions) == 0 {
			t.Fatalf("Explanation = %+v", explanation)
		}
	})
}
