package bytedance

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"

	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
)

type arkResponsesRequest = arkresponses.ResponsesRequest
type arkVideoRequest = arkmodel.CreateContentGenerationTaskRequest

type driver struct {
	settings Settings
	clients  *clients
	text     protocol.GenerateDriver
	stream   protocol.GenerateStreamDriver
	image    protocol.GenerateDriver
	video    protocol.GenerateDriver
	embedder protocol.EmbedderDriver
}

// New builds a provider for one deployment.
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

	driver.text, err = protocol.BindGenerate(
		ProviderID, driver.compile, driver.call, decodeGenerate,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("bytedance: %w", err)
	}
	driver.stream, err = protocol.BindGenerateStream(
		ProviderID, driver.compile, openGenerateStream(clients.ark, clients.arkRequestOptions),
		newGenerateStreamDecoder,
	)
	if err != nil {
		return inference.Provider{}, fmt.Errorf("bytedance: %w", err)
	}
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
		return d.text.Execute(ctx, request)
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
	if request.Output.Kind() != inference.OutputText {
		return nil, inference.NewError(
			inference.UnsupportedOperation, inference.FieldStream,
			fmt.Errorf("bytedance: %q has no streaming surface", request.Output.Kind()))
	}
	return d.stream.Open(ctx, request)
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

func (d *driver) compile(
	ctx context.Context,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (protocol.Compiled[*arkResponsesRequest], error) {
	declared, err := d.descriptor(request.Model.ID)
	if err != nil {
		return protocol.Compiled[*arkResponsesRequest]{}, err
	}
	return compileGenerate(d.clients.endpoint(request.Model.ID.Name), declared)(ctx, request, shape)
}

func (d *driver) call(
	ctx context.Context,
	request inference.GenerateRequest,
	wire *arkResponsesRequest,
) (generateRaw, error) {
	return transportGenerate(d.clients.ark, d.clients.arkRequestOptions)(ctx, request, wire)

}
