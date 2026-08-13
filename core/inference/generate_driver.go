package inference

import "context"

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
	response, report, err := d.pipeline.execute(ctx, model, request)
	if err != nil {
		return GenerateResponse{}, err
	}
	deriveGenerateUsage(request, &response)
	response.Metadata = mergeProviderIDs(report.Metadata(model), response.Metadata)
	return response, nil
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
	response, report, err := d.pipeline.execute(ctx, model, request)
	if err != nil {
		return EmbedResponse{}, err
	}
	response.Usage.ItemCount = len(response.Embeddings)
	response.Metadata = mergeProviderIDs(report.Metadata(model), response.Metadata)
	return response, nil
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
