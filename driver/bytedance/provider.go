package bytedance

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

type arkVideoRequest = arkmodel.CreateContentGenerationTaskRequest

type driver struct {
	settings Settings
	clients  *clients
	image    protocol.GenerateDriver
	video    protocol.GenerateDriver
	embedder protocol.EmbedderDriver
}

// New builds a provider for one deployment.
// ArkOpenAIBaseURL is Ark's OpenAI-compatible root. Text generation lives there:
// a deployment writes `driver: openai` with this base_url instead of using this
// package, which keeps the media surfaces Ark speaks in its own dialect.
const ArkOpenAIBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

func New(settings Settings) (inference.Provider, error) {
	settings = settings.normalized()
	if err := settings.Validate(); err != nil {
		return inference.Provider{}, err
	}
	clients, err := newClients(settings)
	if err != nil {
		return inference.Provider{}, err
	}
	driver := &driver{settings: settings, clients: clients}

	driver.image, err = protocol.BindGenerate(ProviderID, driver.compileImageRequest,
		transportImage(clients), decodeImage)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("bytedance image: %w", err)
	}
	driver.video, err = protocol.BindGenerate(ProviderID, driver.compileVideoRequest,
		transportVideo(clients.ark, settings.VideoPollInterval, clients.arkRequestOptions), decodeVideo)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("bytedance video: %w", err)
	}
	driver.embedder, err = driver.bindEmbedder()
	if err != nil {
		return inference.Provider{}, fmt.Errorf("bytedance: %w", err)
	}
	provider := inference.Provider{
		Generator: driver,
		Embedder:  driver.embedder,
	}
	if len(settings.Models) > 0 {
		provider.Catalog = newCatalog(settings.Models)
	}
	if err := provider.Validate(); err != nil {
		return inference.Provider{}, fmt.Errorf("bytedance: %w", err)
	}
	return provider, nil
}

func (d *driver) compileImageRequest(
	ctx context.Context,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (protocol.Compiled[*imageRequest], error) {
	declared, err := d.descriptor(request.Model.ID)
	if err != nil {
		return protocol.Compiled[*imageRequest]{}, err
	}
	return compileImage(d.clients.endpoint(request.Model.ID.Name), declared)(ctx, request, shape)
}

func (d *driver) compileVideoRequest(
	ctx context.Context,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (protocol.Compiled[*arkVideoRequest], error) {
	declared, err := d.videoSpec(request.Model.ID)
	if err != nil {
		return protocol.Compiled[*arkVideoRequest]{}, err
	}
	return compileVideo(d.clients.endpoint(request.Model.ID.Name), declared)(ctx, request, shape)
}

func (d *driver) videoSpec(id model.ModelID) (ModelSpec, error) {
	for _, declared := range d.settings.Models {
		if declared.Name == id.Name {
			return declared, nil
		}
	}
	return ModelSpec{}, inference.NewError(
		inference.UnknownModel, inference.FieldModel,
		fmt.Errorf("bytedance: model %q is not declared", id.Name),
	)
}

func (d *driver) Execute(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, error) {
	switch request.Output.Kind() {
	case inference.OutputText:
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnsupportedOperation, inference.FieldOutput,
			fmt.Errorf("bytedance: text generation is driver: openai pointed at Ark "+
				"(base_url %s, api chat); this driver serves image, video and embedding",
				ArkOpenAIBaseURL))
	case inference.OutputImage:
		return d.image.Execute(ctx, request)
	case inference.OutputVideo:
		return d.video.Execute(ctx, request)
	default:
		return inference.GenerateResponse{}, inference.NewError(
			inference.UnsupportedOperation, inference.FieldOutput,
			fmt.Errorf("bytedance: output %q is not served", request.Output.Kind()))
	}
}

func (d *driver) Generate(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.GenerateResponse, error) {
	return d.Execute(ctx, request)
}

func (d *driver) GenerateStream(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.Stream, error) {
	return d.Open(ctx, request)
}

func (d *driver) Open(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.Stream, error) {
	return nil, inference.NewError(
		inference.UnsupportedOperation, inference.FieldStream,
		fmt.Errorf("bytedance: %q has no streaming surface; text streaming is "+
			"driver: openai pointed at Ark", request.Output.Kind()))
}

func (d *driver) descriptor(id model.ModelID) (model.ModelDescriptor, error) {
	for _, declared := range d.settings.Models {
		if declared.Name == id.Name {
			return descriptor(declared), nil
		}
	}
	return model.ModelDescriptor{}, inference.NewError(
		inference.UnknownModel, inference.FieldModel,
		fmt.Errorf("bytedance: model %q is not declared", id.Name),
	)
}
