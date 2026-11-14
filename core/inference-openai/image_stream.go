package openai

import (
	"context"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
)

// imageStream adapts the SDK's image event stream to the protocol's wire stream.

// imageOpen starts the streaming call. The SDK turns on `stream: true` itself.
func (d *driver) imageOpen(
	ctx context.Context,
	request inference.GenerateRequest,
	params openai.ImageGenerateParams,
) (protocol.RawStream[openai.ImageGenStreamEventUnion], error) {
	stream := d.client.Images.GenerateStreaming(ctx, params)
	return newSDKStream(stream.Next, stream.Current, stream.Err, stream.Close), nil
}

// imageStreamDecoder builds the per-stream decoder. Partial and final images share
// content index zero on purpose: the assembler keeps one part per index, so the
// finished image replaces the last preview while the caller still observes every
// preview on the way there.
func imageStreamDecoder(request inference.GenerateRequest) protocol.StreamDecoder[openai.ImageGenStreamEventUnion] {
	options, _ := imageOptions(request.Extensions)
	mediaType := imageMediaType(options)
	metadata := inference.Metadata{
		Model: model.ModelID{Provider: ProviderID, Name: request.Model.ID.Name},
	}
	started := false
	return func(_ context.Context, event openai.ImageGenStreamEventUnion) ([]inference.StreamEvent, error) {
		var events []inference.StreamEvent
		if !started {
			started = true
			events = append(events, inference.StreamEvent{
				Kind:     inference.StreamStart,
				Metadata: &metadata,
			})
		}
		switch event.Type {
		case "image_generation.partial_image":
			part, err := b64ImagePart(event.B64JSON, mediaType)
			if err != nil {
				return nil, err
			}
			events = append(events, inference.StreamEvent{
				Kind:  inference.StreamPart,
				Index: imagePartIndex,
				Part:  part,
			})
		case "image_generation.completed":
			part, err := b64ImagePart(event.B64JSON, mediaType)
			if err != nil {
				return nil, err
			}
			accounting := imageStreamUsage(
				event.Usage.InputTokens, event.Usage.OutputTokens,
			)
			events = append(events,
				inference.StreamEvent{Kind: inference.StreamPart, Index: imagePartIndex, Part: part},
				inference.StreamEvent{Kind: inference.StreamUsage, Usage: &accounting},
				inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
			)
		}
		return events, nil
	}
}

// imagePartIndex is the content index images occupy. One image per stream is the
// only shape the API offers, so a single constant is enough.
const imagePartIndex = 0

// imageStreamUsage reports one generated image plus the token accounting.
func imageStreamUsage(input, output int64) usage.Usage {
	count := int64(1)
	return usage.Usage{
		InputTokens:  input,
		OutputTokens: output,
		Work: map[usage.WorkKind]int64{
			usage.WorkImagesGenerated: count,
		},
	}
}
