package openai

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
)

// driver holds one deployment's configuration and its SDK client. The client is
// shared by every call: it is safe for concurrent use and owns connection reuse.
type driver struct {
	settings Settings
	client   openaiClient

	text            protocol.GenerateDriver
	textStream      protocol.GenerateStreamDriver
	image           protocol.GenerateDriver
	imageStream     protocol.GenerateStreamDriver
	imageEdit       protocol.GenerateDriver
	imageEditStream protocol.GenerateStreamDriver
	speech          protocol.GenerateDriver
	speechStream    protocol.GenerateStreamDriver
}

// Execute dispatches on the requested output. The two modalities are separate
// pipelines because the endpoints, parameters and answer shapes share nothing; the
// dispatch is the only place that knows both exist.
func (d *driver) Execute(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, error) {
	if request.Output.Kind() == inference.OutputAudio {
		return d.speech.Execute(ctx, request)
	}
	if request.Output.Kind() == inference.OutputImage {
		// A request carrying a reference image is an edit; one without is a
		// generation. The caller does not choose a surface, only content.
		if hasImageInput(request) {
			return d.imageEdit.Execute(ctx, request)
		}
		return d.image.Execute(ctx, request)
	}
	return d.text.Execute(ctx, request)
}

// Open dispatches streaming the same way unary calls are dispatched.
//
// Image streaming works because a preview and the finished image share a content
// index and the assembler keeps the last one. Speech chunks use the opposite rule —
// they append — which is why the decoder emits [inference.StreamChunk] rather than
// a sequence of parts.
func (d *driver) Open(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.Stream, error) {
	switch request.Output.Kind() {
	case inference.OutputImage:
		if hasImageInput(request) {
			return d.imageEditStream.Open(ctx, request)
		}
		return d.imageStream.Open(ctx, request)
	case inference.OutputAudio:
		return d.speechStream.Open(ctx, request)
	default:
		return d.textStream.Open(ctx, request)
	}
}

// New builds a provider for one deployment. The dialect is a deployment
// decision: a chat deployment and a responses deployment are two providers,
// because the two surfaces accept different parameters for the same request.
//
// Both halves are always bound. A driver that could only answer unary requests
// would push the "this model does not stream" decision into every caller; here it
// stays a property of the deployment.
func New(settings Settings) (inference.Provider, error) {
	normalized := settings.normalized()
	if err := normalized.Validate(); err != nil {
		return nil, err
	}
	d := &driver{settings: normalized, client: newClient(normalized)}

	unary, stream, err := d.bind(normalized.API.Kind())
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	d.text = unary
	d.textStream = stream
	d.image, err = protocol.BindGenerate(
		ProviderID, d.imageCompile, d.imageCall, d.imageDecode,
	)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	d.imageStream, err = protocol.BindGenerateStream(
		ProviderID, d.imageCompile, d.imageOpen, imageStreamDecoder,
	)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	d.imageEdit, err = protocol.BindGenerate(
		ProviderID, d.imageEditCompile, d.imageEditCall, d.imageDecode,
	)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	d.imageEditStream, err = protocol.BindGenerateStream(
		ProviderID, d.imageEditCompile, d.imageEditOpen, imageEditStreamDecoder,
	)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	d.speech, err = protocol.BindGenerate(
		ProviderID, d.speechCompile, d.speechCall, d.speechDecode,
	)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	d.speechStream, err = protocol.BindGenerateStream(
		ProviderID, d.speechCompile, d.speechOpen, speechStreamDecoder,
	)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	if len(normalized.Models) > 0 {
		return protocol.BindProviderWithCatalog(d, d, newCatalog(normalized.Models)), nil
	}
	return protocol.BindProvider(d, d), nil
}

// bind assembles the two pipelines for one dialect.
func (d *driver) bind(api API) (protocol.GenerateDriver, protocol.GenerateStreamDriver, error) {
	switch api {
	case APIChat:
		unary, err := protocol.BindGenerate(ProviderID, d.chatCompile, d.chatCall, d.chatDecode)
		if err != nil {
			return nil, nil, err
		}
		stream, err := protocol.BindGenerateStream(
			ProviderID, d.chatCompile, d.chatOpen, chatStreamDecoder,
		)
		if err != nil {
			return nil, nil, err
		}
		return unary, stream, nil
	default:
		unary, err := protocol.BindGenerate(
			ProviderID, d.responsesCompile, d.responsesCall, d.responsesDecode,
		)
		if err != nil {
			return nil, nil, err
		}
		stream, err := protocol.BindGenerateStream(
			ProviderID, d.responsesCompile, d.responsesOpen, responsesStreamDecoder,
		)
		if err != nil {
			return nil, nil, err
		}
		return unary, stream, nil
	}
}
