package inference

import (
	"context"
	"time"
)

type generateDriver[Wire, Raw any] struct {
	pipeline *pipeline[GenerateRequest, Wire, Raw, GenerateResponse]
	binding  *generateCompilerBinding
}

func (*generateDriver[Wire, Raw]) inferenceGenerateDriver() {}
func (d *generateDriver[Wire, Raw]) generateCompilerBinding() *generateCompilerBinding {
	return d.binding
}

func (d *generateDriver[Wire, Raw]) Explain(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, request)
}

func (d *generateDriver[Wire, Raw]) Execute(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (GenerateResponse, error) {
	prepared, err := d.Prepare(ctx, model, request)
	if err != nil {
		return GenerateResponse{}, err
	}
	return prepared.Execute(ctx)
}

// Prepare compiles one unary request against model and returns a handle that
// executes it without compiling again.
func (d *generateDriver[Wire, Raw]) Prepare(
	ctx context.Context,
	model ModelRef,
	request GenerateRequest,
) (*Prepared[GenerateResponse], error) {
	compiled, err := d.pipeline.prepare(ctx, model, request)
	if err != nil {
		return nil, err
	}
	return newPrepared(
		model,
		OperationGenerate,
		compiled.Report,
		func(runCtx context.Context) (GenerateResponse, error) {
			start := time.Now()
			response, err := d.pipeline.executeCompiled(runCtx, model, request, compiled)
			if err != nil {
				return GenerateResponse{}, err
			}
			deriveGenerateUsage(request, &response)
			// Stamp the call-context envelope the driver never sees: the
			// exact model (including credential profile) that produced the
			// call and the wall-clock latency of the producing call. Hosts
			// bucket and enforce per-model budgets off Usage.Model; without
			// this stamp the field would be zero for every provider.
			response.Usage.Model = model
			response.Usage.LatencyMs = time.Since(start).Milliseconds()
			response.Metadata = mergeProviderIDs(
				compiled.Report.Metadata(model), response.Metadata)
			return response, nil
		},
	), nil
}

type embedDriver[Wire, Raw any] struct {
	pipeline *pipeline[EmbedRequest, Wire, Raw, EmbedResponse]
}

func (*embedDriver[Wire, Raw]) inferenceEmbedDriver() {}

func (d *embedDriver[Wire, Raw]) Explain(
	ctx context.Context,
	model ModelRef,
	request EmbedRequest,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, request)
}

func (d *embedDriver[Wire, Raw]) Execute(
	ctx context.Context,
	model ModelRef,
	request EmbedRequest,
) (EmbedResponse, error) {
	prepared, err := d.Prepare(ctx, model, request)
	if err != nil {
		return EmbedResponse{}, err
	}
	return prepared.Execute(ctx)
}

// Prepare compiles one embedding request against model and returns a handle
// that executes it without compiling again.
func (d *embedDriver[Wire, Raw]) Prepare(
	ctx context.Context,
	model ModelRef,
	request EmbedRequest,
) (*Prepared[EmbedResponse], error) {
	compiled, err := d.pipeline.prepare(ctx, model, request)
	if err != nil {
		return nil, err
	}
	return newPrepared(
		model,
		OperationEmbed,
		compiled.Report,
		func(runCtx context.Context) (EmbedResponse, error) {
			response, err := d.pipeline.executeCompiled(runCtx, model, request, compiled)
			if err != nil {
				return EmbedResponse{}, err
			}
			response.Usage.ItemCount = len(response.Embeddings)
			response.Metadata = mergeProviderIDs(
				compiled.Report.Metadata(model), response.Metadata)
			return response, nil
		},
	), nil
}

// mergeProviderIDs keeps provider-reported request/response identifiers
// when the runtime stamps the operation metadata over the decoder's result.
func mergeProviderIDs(metadata, decoded Metadata) Metadata {
	if decoded.RequestID != "" {
		metadata.RequestID = decoded.RequestID
	}
	if decoded.ResponseID != "" {
		metadata.ResponseID = decoded.ResponseID
	}
	return metadata
}
