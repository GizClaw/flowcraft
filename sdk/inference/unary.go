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
	response.Metadata = report.Metadata(model)
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
	response.Metadata = report.Metadata(model)
	return response, nil
}

type transcriptionDriver[Wire, Raw any] struct {
	pipeline *pipeline[TranscriptionRequest, Wire, Raw, TranscriptionResponse]
}

func (*transcriptionDriver[Wire, Raw]) inferenceTranscriptionDriver() {}

func (d *transcriptionDriver[Wire, Raw]) Explain(
	ctx context.Context,
	model ModelRef,
	request TranscriptionRequest,
) (Explanation, error) {
	return d.pipeline.explain(ctx, model, request)
}

func (d *transcriptionDriver[Wire, Raw]) Execute(
	ctx context.Context,
	model ModelRef,
	request TranscriptionRequest,
) (TranscriptionResponse, error) {
	response, report, err := d.pipeline.execute(ctx, model, request)
	if err != nil {
		return TranscriptionResponse{}, err
	}
	response.Metadata = report.Metadata(model)
	return response, nil
}
