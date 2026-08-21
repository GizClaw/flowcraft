package azure

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func conformanceTextRequest() inference.GenerateRequest {
	return inference.GenerateRequest{Input: inference.GenerateInput{
		Role: inference.InputRoleUser,
		Content: inference.InputContent{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
			Intent:  inference.Intent{Text: &inference.TextIntent{}},
		},
	}}
}

func conformanceModel(name string) inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: driverID, Name: name},
		Profile: "default",
	}
}

func conformanceGenerateDrivers[Wire any](
	t *testing.T,
	compile inference.GenerateCompiler[Wire],
	calls *inferencetest.Counter,
) (inference.GenerateDriver, inference.GenerateStreamDriver) {
	t.Helper()
	operations, err := inference.BindGenerateOperations(
		compile,
		countingTransport(calls, func(_ context.Context, wire Wire) (Wire, error) { return wire, nil }),
		inference.Decoder[Wire, inference.GenerateResponse](
			func(_ context.Context, _ Wire) (inference.GenerateResponse, error) {
				return inference.GenerateResponse{
					Message: message.Message{
						Role:    message.RoleAssistant,
						Content: message.Content{Parts: []message.Part{message.TextPart{Text: "ok"}}},
					},
					FinishReason: inference.FinishCompleted,
				}, nil
			},
		),
		countingTransport(calls, func(_ context.Context, _ Wire) (inference.ProviderStream[inference.GenerateStreamEvent], error) {
			return &conformanceStream{events: []inference.GenerateStreamEvent{
				{PartIndex: 0, Delta: inference.TextPartDelta{Text: "ok"}},
				{FinishReason: inference.FinishCompleted},
			}}, nil
		}),
		inference.GenerateStreamDecoder[inference.GenerateStreamEvent](
			func(_ context.Context, event inference.GenerateStreamEvent) (inference.GenerateStreamEvent, error) {
				return event, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	return operations.Unary, operations.Stream
}

func conformanceEmbedDriver[Wire any](
	t *testing.T,
	compile inference.Compiler[inference.EmbedRequest, Wire],
	calls *inferencetest.Counter,
) inference.EmbedDriver {
	t.Helper()
	driver, err := inference.BindEmbed(
		compile,
		countingTransport(calls, func(_ context.Context, wire Wire) (Wire, error) { return wire, nil }),
		inference.Decoder[Wire, inference.EmbedResponse](
			func(_ context.Context, _ Wire) (inference.EmbedResponse, error) {
				return inference.EmbedResponse{
					Embeddings: []inference.Embedding{{Vector: []float32{1}}},
				}, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindEmbed: %v", err)
	}
	return driver
}

func countingTransport[Wire, Raw any](
	calls *inferencetest.Counter,
	next inference.Transport[Wire, Raw],
) inference.Transport[Wire, Raw] {
	return func(ctx context.Context, wire Wire) (Raw, error) {
		calls.Inc()
		return next(ctx, wire)
	}
}

type conformanceStream struct {
	events []inference.GenerateStreamEvent
	index  int
}

func (s *conformanceStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	if s.index >= len(s.events) {
		return inference.GenerateStreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}
func (*conformanceStream) Close() error { return nil }

func TestConformanceGenerateConcurrent(t *testing.T) {
	calls := &inferencetest.Counter{}
	entry := entryFor(ModelSpec{Name: "deploy", Kind: "generate"})
	unary, stream := conformanceGenerateDrivers(t, compileGenerate("deploy", entry), calls)
	inferencetest.RunGenerateConcurrent(t, inferencetest.GenerateConcurrentSuite{
		Model:      conformanceModel("deploy"),
		Request:    conformanceTextRequest,
		Unary:      unary,
		Stream:     stream,
		Goroutines: 2,
	})
}

var errStreamFailure = errors.New("stream failure")

type failingStream struct{}

func (*failingStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	return inference.GenerateStreamEvent{}, errStreamFailure
}
func (*failingStream) Close() error { return nil }

func TestConformanceGenerateStreamFailure(t *testing.T) {
	calls := &inferencetest.Counter{}
	entry := entryFor(ModelSpec{Name: "deploy", Kind: "generate"})
	driver, err := inference.BindGenerateStream(
		compileGenerate("deploy", entry),
		countingTransport(calls, func(_ context.Context, _ generateWire) (inference.ProviderStream[inference.GenerateStreamEvent], error) {
			return &failingStream{}, nil
		}),
		inference.GenerateStreamDecoder[inference.GenerateStreamEvent](
			func(_ context.Context, event inference.GenerateStreamEvent) (inference.GenerateStreamEvent, error) {
				return event, nil
			},
		),
	)
	if err != nil {
		t.Fatalf("BindGenerateStream: %v", err)
	}
	inferencetest.RunGenerateStreamFailure(t, inferencetest.GenerateStreamFailureSuite{
		Model:          conformanceModel("deploy"),
		Request:        conformanceTextRequest,
		Driver:         driver,
		TransportCalls: calls.Load,
		AssertError: func(t *testing.T, err error) {
			if !errors.Is(err, errStreamFailure) {
				t.Fatalf("stream error = %v", err)
			}
		},
	})
}

func TestConformanceEmbedUnary(t *testing.T) {
	calls := &inferencetest.Counter{}
	deployment := "embed-deploy"
	entry := entryFor(ModelSpec{Name: deployment, Kind: "embed", Dimensions: true})
	driver := conformanceEmbedDriver(t, compileEmbed(deployment, entry), calls)
	inferencetest.RunEmbedUnary(t, inferencetest.EmbedUnarySuite{
		Model: conformanceModel(deployment),
		Request: func() inference.EmbedRequest {
			return inference.EmbedRequest{Items: []inference.EmbedItem{{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
			}}}
		},
		Driver:         driver,
		TransportCalls: calls.Load,
	})
}

func TestConformanceImageCompiler(t *testing.T) {
	imageRequest := func() inference.GenerateRequest {
		return inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "draw a rocket"},
					}},
					Intent: inference.Intent{Image: &inference.ImageIntent{}},
				},
			},
		}
	}

	inferencetest.RunGenerateCompiler(t, inferencetest.GenerateCompilerSuite[imageWire]{
		Model:   conformanceModel("gpt-image-1"),
		Shape:   inference.GenerateExecutionUnary,
		Request: imageRequest,
		Snapshot: func(request inference.GenerateRequest) any {
			return request.Clone()
		},
		Compile: compileImage("gpt-image-1"),
		AssertWire: func(t *testing.T, wire imageWire) {
			if wire.model != "gpt-image-1" || wire.prompt != "draw a rocket" {
				t.Fatalf("wire = %+v", wire)
			}
			if len(wire.images) != 0 || wire.mask.Kind() != "" {
				t.Fatalf("wire reference images = %d, mask = %q",
					len(wire.images), wire.mask.Kind())
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.GenerateRequest]{
			{
				Name: "seed has no parameter",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					seed := int64(7)
					request.Input.Content.Intent.Image.Seed = &seed
					return request
				},
				Field: inference.FieldGenerateIntentImageSeed,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "size outside the gpt-image-2 resolution rules",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					request.Input.Content.Intent.Image.Size = &media.ImageSize{
						Width: 800, Height: 600,
					}
					return request
				},
				Field: inference.FieldGenerateIntentImageSize,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "URL delivery",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					request.Input.Content.Intent.Image.Delivery = media.SourceURL
					return request
				},
				Field: inference.FieldGenerateIntentImageDelivery,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "URL reference image has no upload channel",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					source, err := media.NewImageURL(
						"https://example.com/reference.png",
						"image/png",
					)
					if err != nil {
						t.Fatalf("NewImageURL: %v", err)
					}
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						message.ImagePart{Source: source},
					)
					return request
				},
				Field: inference.FieldGenerateInputImage,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "text intent alongside image",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					request.Input.Content.Intent.Text = &inference.TextIntent{}
					return request
				},
				Field: inference.FieldGenerateIntentText,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "mask requires an inline reference image",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					mask, err := media.NewImageBytes(testPNG, "image/png")
					if err != nil {
						t.Fatalf("NewImageBytes: %v", err)
					}
					request.Extensions = inference.Extensions{
						ImageOptions{Mask: &mask},
					}
					return request
				},
				Field: inference.ExtensionField("mask").Qualify(ImageOptions{}),
				Kind:  inference.InvalidExtension,
			},
			{
				Name: "URL-sourced mask has no upload channel",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					source, err := media.NewImageBytes(testPNG, "image/png")
					if err != nil {
						t.Fatalf("NewImageBytes: %v", err)
					}
					request.Input.Content.Parts = append(
						request.Input.Content.Parts,
						message.ImagePart{Source: source},
					)
					mask, err := media.NewImageURL(
						"https://example.com/mask.png",
						"image/png",
					)
					if err != nil {
						t.Fatalf("NewImageURL: %v", err)
					}
					request.Extensions = inference.Extensions{
						ImageOptions{Mask: &mask},
					}
					return request
				},
				Field: inference.ExtensionField("mask").Qualify(ImageOptions{}),
				Kind:  inference.InvalidExtension,
			},
			{
				Name: "generate extension does not apply to image generation",
				Request: func() inference.GenerateRequest {
					request := imageRequest()
					request.Extensions = inference.Extensions{
						GenerateOptions{WebSearch: &GenerateWebSearch{}},
					}
					return request
				},
				Field: inference.ExtensionField("web_search").Qualify(GenerateOptions{}),
				Kind:  inference.InvalidExtension,
			},
		},
	})
}
