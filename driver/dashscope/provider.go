package dashscope

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
)

type driver struct {
	settings   Spec
	clients    *clients
	text       protocol.GenerateDriver
	stream     protocol.GenerateStreamDriver
	tts        protocol.GenerateDriver
	image      protocol.GenerateDriver
	video      protocol.GenerateDriver
	transcribe protocol.GenerateDriver
	embedder   protocol.EmbedderDriver
}

func New(settings Spec) (inference.Provider, error) {
	settings = settings.normalized()
	if err := settings.Validate(); err != nil {
		return inference.Provider{}, err
	}
	if settings.APIKey == "" {
		return inference.Provider{}, fmt.Errorf("dashscope: api key is required")
	}
	clients, err := newClients(settings)
	if err != nil {
		return inference.Provider{}, err
	}
	d := &driver{settings: settings, clients: clients}
	d.text, err = protocol.BindGenerate(ProviderID, d.compileGenerate, transportGenerate(clients.media), decodeGenerate)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("dashscope: %w", err)
	}
	d.stream, err = protocol.BindGenerateStream(ProviderID, d.compileGenerate, openGenerateStream(clients.media), newGenerateStreamDecoder)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("dashscope stream: %w", err)
	}
	d.transcribe, err = protocol.BindGenerate(ProviderID, d.compileTranscribe, transportTranscribe(clients.media), decodeTranscribe)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("dashscope transcription: %w", err)
	}
	d.video, err = protocol.BindGenerate(ProviderID, d.compileVideo, transportVideo(clients.media), decodeVideo)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("dashscope video: %w", err)
	}
	d.image, err = protocol.BindGenerate(ProviderID, d.compileImage, transportImage(clients.media), decodeImage)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("dashscope image: %w", err)
	}
	d.tts, err = protocol.BindGenerate(ProviderID, d.compileTTS, transportTTS(clients.media), decodeTTS)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("dashscope tts: %w", err)
	}
	d.embedder, err = protocol.BindEmbedder(ProviderID, d.compileEmbed, transportEmbed(clients.media), decodeEmbed)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("dashscope embed: %w", err)
	}
	provider := inference.Provider{Generator: d, Embedder: d.embedder}
	if len(settings.Models) > 0 {
		provider.Catalog = newCatalog(settings.Models)
	}
	if err := provider.Validate(); err != nil {
		return inference.Provider{}, fmt.Errorf("dashscope: %w", err)
	}
	return provider, nil
}

func (s Spec) normalized() Spec {
	if s.BaseURL == "" {
		s.BaseURL = DefaultBaseURL
	}
	if s.HTTPClient == nil {
		s.HTTPClient = nil
	}
	return s
}

func (d *driver) spec(id model.ModelID) (ModelSpec, error) {
	for _, declared := range d.settings.Models {
		if declared.Name == id.Name {
			return declared, nil
		}
	}
	return ModelSpec{}, inference.NewError(inference.UnknownModel, inference.FieldModel, fmt.Errorf("dashscope: model %q is not declared", id.Name))
}

func (d *driver) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	if declared, err := d.spec(request.Model.ID); err == nil && modelKind(declared.Kind) == kindTranscribe {
		return d.transcribe.Execute(ctx, request)
	}
	switch request.Output.Kind() {
	case inference.OutputText:
		return d.text.Execute(ctx, request)
	case inference.OutputImage:
		return d.image.Execute(ctx, request)
	case inference.OutputVideo:
		return d.video.Execute(ctx, request)
	case inference.OutputAudio:
		return d.tts.Execute(ctx, request)
	default:
		return inference.GenerateResponse{}, inference.NewError(inference.UnsupportedOperation, inference.FieldOutput, fmt.Errorf("dashscope: output %q is not served yet", request.Output.Kind()))
	}
}

func (d *driver) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	if request.Output.Kind() != inference.OutputText {
		return nil, inference.NewError(inference.UnsupportedOperation, inference.FieldStream, fmt.Errorf("dashscope: output %q has no stream", request.Output.Kind()))
	}
	return d.stream.Open(ctx, request)
}

const ProviderID = providerID
