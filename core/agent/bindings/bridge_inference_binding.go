package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// inferenceBindings is the script bridge's view of the inference assembly: it
// resolves each model through a BindingCache, so a script that keeps addressing
// the same model reuses its opened drivers instead of rebuilding the provider's
// clients on every call. Script arguments can name any configured model, so the
// cache — not the caller — decides what stays open (least-recently-used, with a
// bounded key space).
//
// Each helper mirrors the Assembly method of the same name: prepare compiles
// once (explain stops there), execute performs the provider round trip.
type inferenceBindings struct {
	cache *inference.BindingCache
}

func (b *inferenceBindings) generate(
	ctx context.Context,
	ref model.ModelRef,
	request inference.GenerateRequest,
) (inference.GenerateResponse, error) {
	prepared, err := b.cache.PrepareGenerate(ctx, ref, request)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	return prepared.Execute(ctx)
}

func (b *inferenceBindings) generateStream(
	ctx context.Context,
	ref model.ModelRef,
	request inference.GenerateRequest,
) (inference.GenerateStream, error) {
	prepared, err := b.cache.PrepareGenerateStream(ctx, ref, request)
	if err != nil {
		return nil, err
	}
	return prepared.Execute(ctx)
}

func (b *inferenceBindings) explainGenerate(
	ctx context.Context,
	ref model.ModelRef,
	request inference.GenerateRequest,
) (inference.Explanation, error) {
	prepared, err := b.cache.PrepareGenerate(ctx, ref, request)
	if err != nil {
		return inference.Explanation{}, err
	}
	return prepared.Explanation(), nil
}

func (b *inferenceBindings) explainGenerateStream(
	ctx context.Context,
	ref model.ModelRef,
	request inference.GenerateRequest,
) (inference.Explanation, error) {
	prepared, err := b.cache.PrepareGenerateStream(ctx, ref, request)
	if err != nil {
		return inference.Explanation{}, err
	}
	return prepared.Explanation(), nil
}

func (b *inferenceBindings) embed(
	ctx context.Context,
	ref model.ModelRef,
	request inference.EmbedRequest,
) (inference.EmbedResponse, error) {
	prepared, err := b.cache.PrepareEmbed(ctx, ref, request)
	if err != nil {
		return inference.EmbedResponse{}, err
	}
	return prepared.Execute(ctx)
}

func (b *inferenceBindings) explainEmbed(
	ctx context.Context,
	ref model.ModelRef,
	request inference.EmbedRequest,
) (inference.Explanation, error) {
	prepared, err := b.cache.PrepareEmbed(ctx, ref, request)
	if err != nil {
		return inference.Explanation{}, err
	}
	return prepared.Explanation(), nil
}

func (b *inferenceBindings) transcribe(
	ctx context.Context,
	ref model.ModelRef,
	request inference.TranscriptionRequest,
) (inference.TranscriptionResponse, error) {
	prepared, err := b.cache.PrepareTranscribe(ctx, ref, request)
	if err != nil {
		return inference.TranscriptionResponse{}, err
	}
	return prepared.Execute(ctx)
}

func (b *inferenceBindings) explainTranscribe(
	ctx context.Context,
	ref model.ModelRef,
	request inference.TranscriptionRequest,
) (inference.Explanation, error) {
	prepared, err := b.cache.PrepareTranscribe(ctx, ref, request)
	if err != nil {
		return inference.Explanation{}, err
	}
	return prepared.Explanation(), nil
}

func (b *inferenceBindings) transcribeSession(
	ctx context.Context,
	ref model.ModelRef,
	request inference.TranscriptionSessionRequest,
) (inference.TranscriptionSession, error) {
	prepared, err := b.cache.PrepareTranscribeSession(ctx, ref, request)
	if err != nil {
		return nil, err
	}
	return prepared.Execute(ctx)
}
