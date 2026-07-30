package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestResponseSchemaCompilationRejectsExternalResourcesWithoutIO(t *testing.T) {
	temp := t.TempDir()
	file := filepath.Join(temp, "schema.json")
	if err := os.WriteFile(file, []byte(`{"type":"string"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var httpCalls int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		httpCalls++
	}))
	defer server.Close()

	tests := []struct {
		name string
		ref  string
	}{
		{name: "file", ref: "file://" + file},
		{name: "http", ref: server.URL + "/schema.json"},
		{name: "relative", ref: "other-schema.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			format := ResponseFormat{
				Kind:   ResponseJSONSchema,
				Name:   "external",
				Schema: json.RawMessage(fmt.Sprintf(`{"$ref":%q}`, tt.ref)),
			}
			if err := format.Validate(); err == nil {
				t.Fatal("ResponseFormat.Validate accepted external $ref")
			}
			compileCalls := 0
			transportCalls := 0
			driver, err := BindGenerate(
				func(context.Context, ModelRef, GenerateRequest, GenerateExecutionShape) (Compiled[string], error) {
					compileCalls++
					return Compiled[string]{}, nil
				},
				func(context.Context, string) (string, error) {
					transportCalls++
					return "", nil
				},
				func(context.Context, string) (GenerateResponse, error) {
					return GenerateResponse{}, nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			request := validGenerateTextRequest()
			request.Input.Content.Intent.Text.Response = &format
			_, err = driver.Execute(
				context.Background(),
				ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
				request,
			)
			if !IsKind(err, InvalidRequest) {
				t.Fatalf("Execute error = %v, want InvalidRequest", err)
			}
			if compileCalls != 0 || transportCalls != 0 {
				t.Fatalf("compiler/transport calls = %d/%d, want 0/0", compileCalls, transportCalls)
			}
			if err := validateGenerateText(`{"value":"x"}`, &format); err == nil {
				t.Fatal("response validation accepted external $ref")
			}
		})
	}
	if httpCalls != 0 {
		t.Fatalf("schema compilation performed %d HTTP requests", httpCalls)
	}
}

func TestResponseSchemaCompilationAllowsInternalReferences(t *testing.T) {
	format := ResponseFormat{
		Kind: ResponseJSONSchema,
		Name: "internal",
		Schema: json.RawMessage(`{
			"$defs":{"answer":{"type":"object","required":["value"],"properties":{"value":{"type":"string"}}}},
			"$ref":"#/$defs/answer"
		}`),
	}
	if err := format.Validate(); err != nil {
		t.Fatalf("ResponseFormat.Validate rejected internal $ref: %v", err)
	}
	if err := validateGenerateText(`{"value":"ok"}`, &format); err != nil {
		t.Fatalf("response validation rejected internal $ref: %v", err)
	}
}

func TestBindGenerateOperationsSharesCompilerAndRejectsMutation(t *testing.T) {
	type wire struct{ Prompt string }
	compileCalls := 0
	compile := func(
		_ context.Context,
		_ ModelRef,
		request GenerateRequest,
		shape GenerateExecutionShape,
	) (Compiled[wire], error) {
		compileCalls++
		request.Input.Content.Parts[0] = TextPart{Text: "mutated"}
		return nativeGenerateCompile(wire{Prompt: "native"})(
			context.Background(),
			ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
			request,
			shape,
		)
	}
	operations, err := BindGenerateOperations(
		compile,
		func(context.Context, wire) (string, error) { return "ok", nil },
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{}, nil
		},
		func(context.Context, wire) (ProviderStream[string], error) {
			return &generateStringStream{}, nil
		},
		func(context.Context, string) (GenerateStreamEvent, error) {
			return GenerateStreamEvent{}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	model := ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}}
	request := validGenerateTextRequest()
	if _, err := operations.Unary.Explain(
		context.Background(),
		model,
		request,
	); !IsKind(err, CompilerContractViolation) {
		t.Fatalf("unary Explain error = %v, want CompilerContractViolation", err)
	}
	if _, err := operations.Stream.Explain(
		context.Background(),
		model,
		request,
	); !IsKind(err, CompilerContractViolation) {
		t.Fatalf("stream Explain error = %v, want CompilerContractViolation", err)
	}
	if compileCalls != 2 {
		t.Fatalf("compiler calls = %d, want 2", compileCalls)
	}
	if got := request.Input.Content.Parts[0].(TextPart).Text; got != "hello" {
		t.Fatalf("caller request mutated to %q", got)
	}
}

func TestGenerateCompilerCanAcceptUnaryAndRejectStreamBeforeTransport(t *testing.T) {
	compileCalls := map[GenerateExecutionShape]int{}
	compiler := func(
		_ context.Context,
		_ ModelRef,
		request GenerateRequest,
		shape GenerateExecutionShape,
	) (Compiled[string], error) {
		compileCalls[shape]++
		if shape == GenerateExecutionStream && request.Input.Content.Intent.Image != nil {
			return Compiled[string]{Report: CompileReport{
					Operation: OperationGenerate,
					Decisions: []Decision{{
						Field:       FieldGenerateIntentImage,
						Disposition: Rejected,
						Reason:      "image output is unary-only",
					}},
				}}, NewError(
					UnsupportedFeature,
					OperationGenerate,
					FieldGenerateIntentImage,
					fmt.Errorf("image output is unary-only"),
				)
		}
		active := request.ActiveFieldsFor(shape)
		decisions := make([]Decision, len(active))
		for index, field := range active {
			decisions[index] = Decision{Field: field, Disposition: Native}
		}
		return Compiled[string]{
			Wire: "wire",
			Report: CompileReport{
				Operation: OperationGenerate,
				Decisions: decisions,
			},
		}, nil
	}
	unaryTransportCalls := 0
	streamTransportCalls := 0
	operations, err := BindGenerateOperations(
		compiler,
		func(context.Context, string) (string, error) {
			unaryTransportCalls++
			return "response", nil
		},
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{
				Message: Message{
					Role:    RoleAssistant,
					Content: Content{Parts: []Part{TextPart{Text: "partial"}}},
				},
				FinishReason: FinishMaxOutput,
			}, nil
		},
		func(context.Context, string) (ProviderStream[string], error) {
			streamTransportCalls++
			return &generateStringStream{}, nil
		},
		func(context.Context, string) (GenerateStreamEvent, error) {
			return GenerateStreamEvent{}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	request := validGenerateTextRequest()
	request.Input.Content.Intent.Image = &ImageIntent{}
	model := ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}}
	if _, err := operations.Unary.Execute(context.Background(), model, request); err != nil {
		t.Fatalf("unary Execute: %v", err)
	}
	if _, err := operations.Stream.Stream(context.Background(), model, request); !IsKind(err, UnsupportedFeature) {
		t.Fatalf("stream Stream error = %v, want UnsupportedFeature", err)
	}
	if unaryTransportCalls != 1 || streamTransportCalls != 0 {
		t.Fatalf(
			"unary/stream transport calls = %d/%d, want 1/0",
			unaryTransportCalls,
			streamTransportCalls,
		)
	}
	if compileCalls[GenerateExecutionUnary] != 1 ||
		compileCalls[GenerateExecutionStream] != 1 {
		t.Fatalf("compiler shape calls = %+v", compileCalls)
	}
}

func TestProviderWireRejectsCanonicalGenerateValuesAndOpenInterfaces(t *testing.T) {
	type containsContent struct{ Value Content }
	type containsInputContent struct{ Value InputContent }
	type containsIntent struct{ Value Intent }
	type containsPart struct{ Value Part }
	tests := []struct {
		name string
		bind func() error
	}{
		{
			name: "generate_request",
			bind: func() error {
				_, err := BindGenerate(
					func(context.Context, ModelRef, GenerateRequest, GenerateExecutionShape) (Compiled[GenerateRequest], error) {
						return Compiled[GenerateRequest]{}, nil
					},
					func(context.Context, GenerateRequest) (string, error) { return "", nil },
					func(context.Context, string) (GenerateResponse, error) {
						return GenerateResponse{}, nil
					},
				)
				return err
			},
		},
		{
			name: "content",
			bind: func() error {
				_, err := BindGenerate(
					func(context.Context, ModelRef, GenerateRequest, GenerateExecutionShape) (Compiled[containsContent], error) {
						return Compiled[containsContent]{}, nil
					},
					func(context.Context, containsContent) (string, error) { return "", nil },
					func(context.Context, string) (GenerateResponse, error) {
						return GenerateResponse{}, nil
					},
				)
				return err
			},
		},
		{
			name: "input_content",
			bind: func() error {
				_, err := BindGenerate(
					func(context.Context, ModelRef, GenerateRequest, GenerateExecutionShape) (Compiled[containsInputContent], error) {
						return Compiled[containsInputContent]{}, nil
					},
					func(context.Context, containsInputContent) (string, error) { return "", nil },
					func(context.Context, string) (GenerateResponse, error) {
						return GenerateResponse{}, nil
					},
				)
				return err
			},
		},
		{
			name: "intent",
			bind: func() error {
				_, err := BindGenerate(
					func(context.Context, ModelRef, GenerateRequest, GenerateExecutionShape) (Compiled[containsIntent], error) {
						return Compiled[containsIntent]{}, nil
					},
					func(context.Context, containsIntent) (string, error) { return "", nil },
					func(context.Context, string) (GenerateResponse, error) {
						return GenerateResponse{}, nil
					},
				)
				return err
			},
		},
		{
			name: "part_interface",
			bind: func() error {
				_, err := BindGenerate(
					func(context.Context, ModelRef, GenerateRequest, GenerateExecutionShape) (Compiled[containsPart], error) {
						return Compiled[containsPart]{}, nil
					},
					func(context.Context, containsPart) (string, error) { return "", nil },
					func(context.Context, string) (GenerateResponse, error) {
						return GenerateResponse{}, nil
					},
				)
				return err
			},
		},
		{
			name: "open_interface",
			bind: func() error {
				_, err := BindGenerate(
					func(context.Context, ModelRef, GenerateRequest, GenerateExecutionShape) (Compiled[struct{ Value any }], error) {
						return Compiled[struct{ Value any }]{}, nil
					},
					func(context.Context, struct{ Value any }) (string, error) { return "", nil },
					func(context.Context, string) (GenerateResponse, error) {
						return GenerateResponse{}, nil
					},
				)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.bind(); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Bind error = %v, want validation", err)
			}
		})
	}
}

func TestProviderWireRejectsEveryConcreteCanonicalPartThroughNesting(t *testing.T) {
	type nested[T any] struct {
		Values []*[]T
	}
	tests := []struct {
		name string
		bind func() error
	}{
		{"text", func() error { return bindLeakingGenerateWire[nested[TextPart]]() }},
		{"image", func() error { return bindLeakingGenerateWire[nested[ImagePart]]() }},
		{"audio", func() error { return bindLeakingGenerateWire[nested[AudioPart]]() }},
		{"video", func() error { return bindLeakingGenerateWire[nested[VideoPart]]() }},
		{"file", func() error { return bindLeakingGenerateWire[nested[FilePart]]() }},
		{"data", func() error { return bindLeakingGenerateWire[nested[DataPart]]() }},
		{"tool_call", func() error { return bindLeakingGenerateWire[nested[ToolCallPart]]() }},
		{"tool_result", func() error { return bindLeakingGenerateWire[nested[ToolResultPart]]() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.bind(); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("BindGenerate error = %v, want validation", err)
			}
		})
	}
}

func TestProviderWireRejectsNestedCanonicalRequestDTOs(t *testing.T) {
	type nested[T any] struct {
		Values map[string][]*T
	}
	tests := []struct {
		name string
		bind func() error
	}{
		{"message", func() error { return bindLeakingGenerateWire[nested[Message]]() }},
		{"generate_input", func() error { return bindLeakingGenerateWire[nested[GenerateInput]]() }},
		{"text_intent", func() error { return bindLeakingGenerateWire[nested[TextIntent]]() }},
		{"image_intent", func() error { return bindLeakingGenerateWire[nested[ImageIntent]]() }},
		{"audio_intent", func() error { return bindLeakingGenerateWire[nested[AudioIntent]]() }},
		{"response_format", func() error { return bindLeakingGenerateWire[nested[ResponseFormat]]() }},
		{"tool_choice", func() error { return bindLeakingGenerateWire[nested[ToolChoice]]() }},
		{"embed_item", func() error { return bindLeakingGenerateWire[nested[EmbedItem]]() }},
		{"embed_request", func() error { return bindLeakingGenerateWire[nested[EmbedRequest]]() }},
		{"transcription_request", func() error {
			return bindLeakingGenerateWire[nested[TranscriptionRequest]]()
		}},
		{"transcription_session_config", func() error {
			return bindLeakingGenerateWire[nested[TranscriptionSessionConfig]]()
		}},
		{"realtime_config", func() error { return bindLeakingGenerateWire[nested[RealtimeConfig]]() }},
		{"realtime_text_input", func() error {
			return bindLeakingGenerateWire[nested[RealtimeTextInput]]()
		}},
		{"realtime_audio_input", func() error {
			return bindLeakingGenerateWire[nested[RealtimeAudioInput]]()
		}},
		{"realtime_video_input", func() error {
			return bindLeakingGenerateWire[nested[RealtimeVideoInput]]()
		}},
		{"realtime_tool_result_input", func() error {
			return bindLeakingGenerateWire[nested[RealtimeToolResultInput]]()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.bind(); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("BindGenerate error = %v, want validation", err)
			}
		})
	}
}

func bindLeakingGenerateWire[Wire any]() error {
	_, err := BindGenerate(
		func(context.Context, ModelRef, GenerateRequest, GenerateExecutionShape) (Compiled[Wire], error) {
			return Compiled[Wire]{}, nil
		},
		func(context.Context, Wire) (string, error) { return "", nil },
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{}, nil
		},
	)
	return err
}

func TestGenerateOperationsValidateRejectsIndependentDualCompilers(t *testing.T) {
	compile := nativeGenerateCompile("wire")
	unary, err := BindGenerate(
		compile,
		func(context.Context, string) (string, error) { return "", nil },
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := BindGenerateStream(
		compile,
		func(context.Context, string) (ProviderStream[string], error) {
			return &generateStringStream{}, nil
		},
		func(context.Context, string) (GenerateStreamEvent, error) {
			return GenerateStreamEvent{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := (GenerateOperations{Unary: unary}).Validate(); err != nil {
		t.Fatalf("unary-only operations rejected: %v", err)
	}
	if err := (GenerateOperations{Stream: stream}).Validate(); err != nil {
		t.Fatalf("stream-only operations rejected: %v", err)
	}
	if err := (GenerateOperations{Unary: unary, Stream: stream}).Validate(); err == nil {
		t.Fatal("independently bound dual operations accepted")
	}

	shared, err := BindGenerateOperations(
		compile,
		func(context.Context, string) (string, error) { return "", nil },
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{}, nil
		},
		func(context.Context, string) (ProviderStream[string], error) {
			return &generateStringStream{}, nil
		},
		func(context.Context, string) (GenerateStreamEvent, error) {
			return GenerateStreamEvent{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.Validate(); err != nil {
		t.Fatalf("shared operations rejected: %v", err)
	}
}

func TestGenerateRejectsUncompilableResponseSchemaBeforeCompilerAndTransport(t *testing.T) {
	compileCalls := 0
	transportCalls := 0
	driver, err := BindGenerate(
		func(context.Context, ModelRef, GenerateRequest, GenerateExecutionShape) (Compiled[string], error) {
			compileCalls++
			return Compiled[string]{}, nil
		},
		func(context.Context, string) (string, error) {
			transportCalls++
			return "", nil
		},
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := validGenerateTextRequest()
	request.Input.Content.Intent.Text.Response = &ResponseFormat{
		Kind:   ResponseJSONSchema,
		Name:   "broken",
		Schema: json.RawMessage(`{"type":"not-a-schema-type"}`),
	}
	_, err = driver.Execute(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
		request,
	)
	if !IsKind(err, InvalidRequest) {
		t.Fatalf("Execute error = %v, want InvalidRequest", err)
	}
	if compileCalls != 0 || transportCalls != 0 {
		t.Fatalf("compiler/transport calls = %d/%d, want 0/0", compileCalls, transportCalls)
	}
}
