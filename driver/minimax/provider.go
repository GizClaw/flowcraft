package minimax

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
)

// driver serves one deployment: the speech surface (unary and streamed) and the
// H3 video task surface.
type driver struct {
	settings  Settings
	clients   *clients
	tts       protocol.GenerateDriver
	ttsStream protocol.GenerateStreamDriver
	video     protocol.GenerateDriver
}

// clients bundles the handles one deployment opens drivers with. Every surface
// is a MiniMax-native JSON API, so one Bearer client rooted at the media base
// URL is all it takes.
type clients struct {
	media *mediaClient
}

// New builds a provider for one deployment.
func New(settings Settings) (inference.Provider, error) {
	settings = settings.normalized()
	if err := settings.Validate(); err != nil {
		return inference.Provider{}, err
	}
	built := &clients{media: newMediaClient(settings)}
	d := &driver{settings: settings, clients: built}

	var err error
	d.video, err = protocol.BindGenerate(
		ProviderID, d.compileVideo, transportVideo(built.media), decodeVideo,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("minimax video: %w", err)
	}
	d.tts, err = protocol.BindGenerate(
		ProviderID, d.compileTTS, transportTTS(built.media), decodeTTS,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("minimax tts: %w", err)
	}
	d.ttsStream, err = protocol.BindGenerateStream(
		ProviderID, d.compileTTS, openTTSStream(built.media), newTTSStreamDecoder,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("minimax tts stream: %w", err)
	}

	provider := inference.Provider{Generator: d}
	if len(settings.Models) > 0 {
		provider.Catalog = newCatalog(settings.Models)
	}
	if err := provider.Validate(); err != nil {
		return inference.Provider{}, fmt.Errorf("minimax: %w", err)
	}
	return provider, nil
}

// spec resolves the declaration serving one model. An undeclared model fails
// closed: the driver ships no line-up, so a name it does not know is not a name
// it can price, shape, or validate.
func (d *driver) spec(id model.ModelID) (ModelSpec, error) {
	for _, declared := range d.settings.Models {
		if declared.Name == id.Name {
			return declared, nil
		}
	}
	return ModelSpec{}, inference.NewError(
		inference.UnknownModel, inference.FieldModel,
		fmt.Errorf("minimax: model %q is not declared", id.Name))
}

func (d *driver) compileTTS(
	ctx context.Context,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (protocol.Compiled[ttsWire], error) {
	declared, err := d.spec(request.Model.ID)
	if err != nil {
		return protocol.Compiled[ttsWire]{}, err
	}
	return compileTTS(declared, request.Model.ID.Name)(ctx, request, shape)
}

func (d *driver) compileVideo(
	ctx context.Context,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (protocol.Compiled[videoWire], error) {
	declared, err := d.spec(request.Model.ID)
	if err != nil {
		return protocol.Compiled[videoWire]{}, err
	}
	return compileVideo(declared, request.Model.ID.Name)(ctx, request, shape)
}

// Generate dispatches one unary call to the surface its declared model serves.
// The declaration decides, not the output alone: a speech model asked for video
// reports the family mismatch rather than silently billing the wrong endpoint.
func (d *driver) Generate(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, error) {
	declared, err := d.spec(request.Model.ID)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	switch modelKind(declared.Kind) {
	case kindTTS:
		return d.tts.Execute(ctx, request)
	case kindVideo:
		return d.video.Execute(ctx, request)
	default:
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnsupportedOperation, inference.FieldModel,
			fmt.Errorf("minimax: model %q declares kind %q, which no surface serves",
				declared.Name, declared.Kind))
	}
}

// GenerateStream serves streamed speech. The video task API is unary, so a video
// request is rejected here rather than opened and failed later.
func (d *driver) GenerateStream(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.Stream, error) {
	declared, err := d.spec(request.Model.ID)
	if err != nil {
		return nil, err
	}
	if modelKind(declared.Kind) != kindTTS {
		return nil, inference.NewError(
			inference.UnsupportedOperation, inference.FieldStream,
			fmt.Errorf("minimax: model %q has no streaming surface", declared.Name))
	}
	if request.Output.Kind() != inference.OutputAudio {
		return nil, inference.NewError(
			inference.UnsupportedOperation, inference.FieldOutput,
			fmt.Errorf("minimax: output %q has no streaming surface", request.Output.Kind()))
	}
	return d.ttsStream.Open(ctx, request)
}
