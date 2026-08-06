package inference

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

func newRuntimeGenerateOperations(
	t *testing.T,
	unaryCalls *atomic.Int32,
) GenerateOperations {
	t.Helper()
	operations, err := BindGenerateOperations(
		nativeGenerateCompile("wire"),
		func(context.Context, string) (string, error) {
			unaryCalls.Add(1)
			return "response", nil
		},
		func(context.Context, string) (GenerateResponse, error) {
			return GenerateResponse{
				Message: message.Message{
					Role:    message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{message.TextPart{Text: "ok"}}},
				},
				FinishReason: FinishCompleted,
			}, nil
		},
		func(context.Context, string) (ProviderStream[GenerateStreamEvent], error) {
			return &generateEventStream{events: []GenerateStreamEvent{
				{PartIndex: 0, Delta: TextPartDelta{Text: "ok"}},
				{FinishReason: FinishCompleted},
			}}, nil
		},
		func(_ context.Context, event GenerateStreamEvent) (GenerateStreamEvent, error) {
			return event, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	return operations
}

func TestRuntimeGenerateUsesExactTargetPolicyCacheAndMetadata(t *testing.T) {
	var opens atomic.Int32
	var unaryCalls atomic.Int32
	var openedModel ModelRef
	var policyModels []ModelRef
	defaultTokens := 32
	constrainedTokens := 16
	operations := newRuntimeGenerateOperations(t, &unaryCalls)
	runtime, err := NewRuntime(
		[]ProviderDefinition{{
			ID: "fake",
			Profiles: []ProfileDefinition{{
				ID:         "tenant-a",
				Operations: []Operation{OperationGenerate},
			}},
			Models: []ModelImplementation{{
				Descriptor: ModelDescriptor{
					ID: ModelID{Provider: "fake", Name: "omni"},
				},
				Openers: Openers{
					Generate: func(_ context.Context, model ModelRef) (GenerateOperations, error) {
						opens.Add(1)
						openedModel = model
						return operations, nil
					},
				},
			}},
		}},
		WithRequestPolicies(RequestPolicies{
			Generate: RequestPolicy[GenerateRequest]{
				Defaults: func(
					_ context.Context,
					model ModelRef,
					request GenerateRequest,
				) GenerateRequest {
					policyModels = append(policyModels, model)
					request.Input.Content.Intent.Text.MaxOutputTokens = &defaultTokens
					return request
				},
				Constraints: func(
					_ context.Context,
					model ModelRef,
					request GenerateRequest,
				) (GenerateRequest, error) {
					policyModels = append(policyModels, model)
					request.Input.Content.Intent.Text.MaxOutputTokens = &constrainedTokens
					return request, nil
				},
				Policy: func(
					_ context.Context,
					model ModelRef,
					request GenerateRequest,
				) (GenerateRequest, error) {
					policyModels = append(policyModels, model)
					if request.Input.Content.Intent.Text.MaxOutputTokens == nil ||
						*request.Input.Content.Intent.Text.MaxOutputTokens != constrainedTokens {
						return request, errors.New("constraints missing")
					}
					return request, nil
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	model := ModelRef{
		ID:      ModelID{Provider: "fake", Name: "omni"},
		Profile: "tenant-a",
	}
	response, err := runtime.Generate(context.Background(), model, validGenerateTextRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Metadata.Model != model.ID ||
		response.Metadata.Operation != OperationGenerate {
		t.Fatalf("metadata = %+v", response.Metadata)
	}
	if openedModel != model || len(policyModels) != 3 {
		t.Fatalf("opened=%+v policies=%+v, want exact target %+v", openedModel, policyModels, model)
	}
	if _, err := runtime.ExplainGenerate(
		context.Background(),
		model,
		validGenerateTextRequest(),
	); err != nil {
		t.Fatalf("ExplainGenerate: %v", err)
	}
	if opens.Load() != 1 {
		t.Fatalf("opens = %d, want cached 1", opens.Load())
	}
	if err := runtime.Invalidate(Invalidation{Provider: "fake"}); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := runtime.ExplainGenerate(
		context.Background(),
		model,
		validGenerateTextRequest(),
	); err != nil {
		t.Fatalf("ExplainGenerate after invalidation: %v", err)
	}
	if opens.Load() != 2 {
		t.Fatalf("opens after invalidation = %d, want 2", opens.Load())
	}
	models, err := runtime.Models("fake")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 ||
		len(models[0].Operations) != 1 ||
		models[0].Operations[0] != OperationGenerate {
		t.Fatalf("model operations = %+v", models)
	}
}

func TestRuntimeOpenerCompletesAfterCallerCancellationAndCachesResult(t *testing.T) {
	var opens atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var unaryCalls atomic.Int32
	operations := newRuntimeGenerateOperations(t, &unaryCalls)
	runtime, err := NewRuntime([]ProviderDefinition{{
		ID: "fake",
		Models: []ModelImplementation{{
			Descriptor: ModelDescriptor{
				ID: ModelID{Provider: "fake", Name: "context"},
			},
			Openers: Openers{
				Generate: func(context.Context, ModelRef) (GenerateOperations, error) {
					if opens.Add(1) == 1 {
						close(started)
					}
					<-release
					return operations, nil
				},
			},
		}},
	}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, callErr := runtime.ExplainGenerate(
			ctx,
			ModelRef{ID: ModelID{Provider: "fake", Name: "context"}},
			validGenerateTextRequest(),
		)
		firstDone <- callErr
	}()
	<-started
	cancel()
	if err := <-firstDone; !IsKind(err, OperationInterrupted) ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("first ExplainGenerate error = %v, want canceled interruption", err)
	}
	close(release)
	if _, err := runtime.ExplainGenerate(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "context"}},
		validGenerateTextRequest(),
	); err != nil {
		t.Fatalf("second ExplainGenerate: %v", err)
	}
	if opens.Load() != 1 {
		t.Fatalf("opener calls = %d, want one shared cached open", opens.Load())
	}
}

func TestRuntimeGenerateStreamDoesNotFallbackToUnary(t *testing.T) {
	var unaryCalls atomic.Int32
	operations := newRuntimeGenerateOperations(t, &unaryCalls)
	operations.Stream = nil
	runtime, err := NewRuntime([]ProviderDefinition{{
		ID: "fake",
		Models: []ModelImplementation{{
			Descriptor: ModelDescriptor{
				ID: ModelID{Provider: "fake", Name: "unary-only"},
			},
			Openers: Openers{
				Generate: func(context.Context, ModelRef) (GenerateOperations, error) {
					return operations, nil
				},
			},
		}},
	}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	_, err = runtime.GenerateStream(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "unary-only"}},
		validGenerateTextRequest(),
	)
	if !IsKind(err, UnsupportedOperation) {
		t.Fatalf("GenerateStream error = %v, want UnsupportedOperation", err)
	}
	if unaryCalls.Load() != 0 {
		t.Fatalf("unary transport calls = %d, want 0", unaryCalls.Load())
	}
}

func TestGenerateDriversDeriveDeterministicMediaUsage(t *testing.T) {
	image, _ := media.NewImageBytes([]byte("image"), "image/png")
	audio, _ := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	duration := int64(250)
	report := int64(99)
	request := validGenerateTextRequest()
	request.Input.Content.Intent.Image = &ImageIntent{}
	request.Input.Content.Intent.Audio = &AudioIntent{
		Voice:  media.VoiceSpec{ID: "alloy"},
		Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
	}
	response := GenerateResponse{
		Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "ok"},
			message.ImagePart{Source: image},
			message.AudioPart{
				Source: audio, Format: &media.AudioFormat{Encoding: media.AudioEncodingMP3},
				DurationMillis: &duration,
			},
		}}},
		FinishReason: FinishCompleted,
		Usage: Usage{
			GeneratedImages:     &report,
			AudioDurationMillis: &report,
		},
	}
	unary, err := BindGenerate(
		nativeGenerateCompile("wire"),
		func(context.Context, string) (string, error) { return "raw", nil },
		func(context.Context, string) (GenerateResponse, error) { return response, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unary.Execute(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "media"}},
		request,
	)
	if err != nil {
		t.Fatalf("unary Execute: %v", err)
	}
	if got.Usage.GeneratedImages == nil || *got.Usage.GeneratedImages != 1 ||
		got.Usage.AudioDurationMillis == nil || *got.Usage.AudioDurationMillis != duration {
		t.Fatalf("unary derived usage = %+v", got.Usage)
	}

	stream, err := BindGenerateStream(
		nativeGenerateCompile("wire"),
		func(context.Context, string) (ProviderStream[GenerateStreamEvent], error) {
			return &generateEventStream{events: []GenerateStreamEvent{
				{Usage: &Usage{GeneratedImages: &report, AudioDurationMillis: &report}},
				{PartIndex: 0, Delta: TextPartDelta{Text: "ok"}},
				{PartIndex: 1, Delta: ImagePartDelta{Part: message.ImagePart{Source: image}}},
				{PartIndex: 2, Delta: AudioPartDelta{
					Data: []byte("audio"), Format: &media.AudioFormat{Encoding: media.AudioEncodingMP3},
					DurationMillis: &duration,
				}},
				{FinishReason: FinishCompleted},
			}}, nil
		},
		func(_ context.Context, event GenerateStreamEvent) (GenerateStreamEvent, error) {
			return event, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := stream.Stream(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "media"}},
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := opened.Next(context.Background()); err != nil {
			if err != io.EOF {
				t.Fatalf("stream Next: %v", err)
			}
			break
		}
	}
	got, err = opened.Result()
	if err != nil {
		t.Fatalf("stream Result: %v", err)
	}
	if got.Usage.GeneratedImages == nil || *got.Usage.GeneratedImages != 1 ||
		got.Usage.AudioDurationMillis == nil || *got.Usage.AudioDurationMillis != duration {
		t.Fatalf("stream derived usage = %+v", got.Usage)
	}
}

func TestGenerateMediaUsageClearsUnknownOrAbsentDerivedValues(t *testing.T) {
	audio, _ := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	report := int64(99)
	request := validGenerateTextRequest()
	request.Input.Content.Intent.Image = &ImageIntent{}
	response := GenerateResponse{
		Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "ok"},
			message.AudioPart{Source: audio},
		}}},
		FinishReason: FinishMaxOutput,
		Usage: Usage{
			GeneratedImages:     &report,
			AudioDurationMillis: &report,
		},
	}
	deriveGenerateUsage(request, &response)
	if response.Usage.GeneratedImages == nil || *response.Usage.GeneratedImages != 0 {
		t.Fatalf("requested image count = %v, want explicit zero", response.Usage.GeneratedImages)
	}
	if response.Usage.AudioDurationMillis != nil {
		t.Fatalf("unknown audio duration = %v, want nil", response.Usage.AudioDurationMillis)
	}
}

func TestRuntimeRejectsIndependentlyBoundDualGenerateOperations(t *testing.T) {
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
	model := ModelRef{ID: ModelID{Provider: "fake", Name: "split"}}
	runtime, err := NewRuntime([]ProviderDefinition{{
		ID: "fake",
		Models: []ModelImplementation{{
			Descriptor: ModelDescriptor{ID: model.ID},
			Openers: Openers{Generate: func(context.Context, ModelRef) (GenerateOperations, error) {
				return GenerateOperations{Unary: unary, Stream: stream}, nil
			}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ExplainGenerate(
		context.Background(), model, validGenerateTextRequest(),
	); !IsKind(err, CompilerContractViolation) {
		t.Fatalf("ExplainGenerate error = %v, want CompilerContractViolation", err)
	}
}
