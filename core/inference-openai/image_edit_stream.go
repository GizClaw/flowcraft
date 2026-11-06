package openai

import (
	"context"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
)

// imageEditStream adapts the SDK's edit event stream to the protocol's wire
// stream.

// imageEditOpen starts the streaming edit call.
func (d *driver) imageEditOpen(
	ctx context.Context,
	request inference.GenerateRequest,
	params openai.ImageEditParams,
) (protocol.RawStream[openai.ImageEditStreamEventUnion], error) {
	auth, err := d.authOptions(request.Model.Profile)
	if err != nil {
		return nil, err
	}
	stream := d.client.Images.EditStreaming(ctx, params, auth...)
	return newSDKStream(stream.Next, stream.Current, stream.Err, stream.Close), nil
}

// imageEditStreamDecoder maps edit events onto canonical events. The shape matches
// the generation stream: previews replace each other at index zero and the
// finished image replaces the last preview.
func imageEditStreamDecoder(request inference.GenerateRequest) protocol.StreamDecoder[openai.ImageEditStreamEventUnion] {
	options, _ := imageOptions(request.Extensions)
	mediaType := imageMediaType(options)
	metadata := inference.Metadata{
		Model: model.ModelID{Provider: ProviderID, Name: request.Model.ID.Name},
	}
	started := false
	return func(_ context.Context, event openai.ImageEditStreamEventUnion) ([]inference.StreamEvent, error) {
		var events []inference.StreamEvent
		if !started {
			started = true
			events = append(events, inference.StreamEvent{
				Kind:     inference.StreamStart,
				Metadata: &metadata,
			})
		}
		switch event.Type {
		case "image_edit.partial_image":
			part, err := b64ImagePart(event.B64JSON, mediaType)
			if err != nil {
				return nil, err
			}
			events = append(events, inference.StreamEvent{
				Kind:  inference.StreamPart,
				Index: imagePartIndex,
				Part:  part,
			})
		case "image_edit.completed":
			part, err := b64ImagePart(event.B64JSON, mediaType)
			if err != nil {
				return nil, err
			}
			accounting := imageStreamUsage(
				event.Usage.InputTokens, event.Usage.OutputTokens, event.Usage.TotalTokens,
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
