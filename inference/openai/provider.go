package openai

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/inference/internal/cache"
	"github.com/GizClaw/flowcraft/inference/pipeline"
)

// driver holds one deployment's configuration and its SDK client. The client is
// shared by every call: it is safe for concurrent use and owns connection reuse.
type driver struct {
	settings Settings
	client   openaiClient
	// schemas memoizes decoded tool schemas for this deployment. It is per instance,
	// not global: one process can serve several providers and none of them should be
	// able to evict another's entries. The bound comes from the settings.
	schemas *cache.LRU[map[string]any]

	text            pipeline.GenerateDriver
	textStream      pipeline.GenerateStreamDriver
	image           pipeline.GenerateDriver
	imageStream     pipeline.GenerateStreamDriver
	imageEdit       pipeline.GenerateDriver
	imageEditStream pipeline.GenerateStreamDriver
	speech          pipeline.GenerateDriver
	transcribe      pipeline.GenerateDriver
	embed           pipeline.EmbedderDriver
	decide          pipeline.DeciderDriver
	speechStream    pipeline.GenerateStreamDriver
}

// Execute dispatches on the requested output. The two modalities are separate
// pipelines because the endpoints, parameters and answer shapes share nothing; the
// dispatch is the only place that knows both exist.
func (d *driver) Execute(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, error) {
	// Audio input selects transcription: the caller asked for text, and the
	// transcription endpoint is the only surface that turns audio into text.
	if hasAudioInput(request) {
		return d.transcribe.Execute(ctx, request)
	}
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
		return inference.Provider{}, err
	}
	d := &driver{
		settings: normalized,
		client:   newClient(normalized),
		schemas:  cache.New[map[string]any](normalized.SchemaCacheEntries),
	}

	unary, stream, err := d.bind(normalized.API.Kind())
	if err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}
	d.text = unary
	d.textStream = stream
	d.image, err = pipeline.BindGenerate(
		ProviderID, d.imageCompile, d.imageCall, d.imageDecode,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}
	d.imageStream, err = pipeline.BindGenerateStream(
		ProviderID, d.imageCompile, d.imageOpen, imageStreamDecoder,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}
	d.imageEdit, err = pipeline.BindGenerate(
		ProviderID, d.imageEditCompile, d.imageEditCall, d.imageDecode,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}
	d.imageEditStream, err = pipeline.BindGenerateStream(
		ProviderID, d.imageEditCompile, d.imageEditOpen, imageEditStreamDecoder,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}
	d.speech, err = pipeline.BindGenerate(
		ProviderID, d.speechCompile, d.speechCall, d.speechDecode,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}
	d.speechStream, err = pipeline.BindGenerateStream(
		ProviderID, d.speechCompile, d.speechOpen, speechStreamDecoder,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}
	d.transcribe, err = pipeline.BindGenerate(
		ProviderID, d.transcribeCompile, d.transcribeCall, d.transcribeDecode,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}
	d.embed, err = pipeline.BindEmbedder(
		ProviderID, d.embedCompile, d.embedCall, d.embedDecode,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}

	d.decide, err = pipeline.BindDecider(
		ProviderID, d.decideCompile, d.decideCall, d.decideDecode,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}

	generated := pipeline.BindGenerator(d, d)
	provider := inference.Provider{
		Generator: generated,
		Embedder:  d.embed,
		Decider:   d.decide,
	}
	if len(normalized.Models) > 0 {
		provider.Catalog = newCatalog(normalized.Models)
	}
	if err := provider.Validate(); err != nil {
		return inference.Provider{}, fmt.Errorf("openai: %w", err)
	}
	return provider, nil
}

// bind assembles the two pipelines for one dialect.
func (d *driver) bind(api API) (pipeline.GenerateDriver, pipeline.GenerateStreamDriver, error) {
	switch api {
	case APIChat:
		unary, err := pipeline.BindGenerate(ProviderID, d.chatCompile, d.chatCall, d.chatDecode)
		if err != nil {
			return nil, nil, err
		}
		stream, err := pipeline.BindGenerateStream(
			ProviderID, d.chatCompile, d.chatOpen, chatStreamDecoder,
		)
		if err != nil {
			return nil, nil, err
		}
		return unary, stream, nil
	default:
		unary, err := pipeline.BindGenerate(
			ProviderID, d.responsesCompile, d.responsesCall, d.responsesDecode,
		)
		if err != nil {
			return nil, nil, err
		}
		stream, err := pipeline.BindGenerateStream(
			ProviderID, d.responsesCompile, d.responsesOpen, responsesStreamDecoder,
		)
		if err != nil {
			return nil, nil, err
		}
		return unary, stream, nil
	}
}
