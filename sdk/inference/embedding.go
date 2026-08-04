package inference

import (
	"fmt"
	"math"

	"github.com/GizClaw/flowcraft/sdk/message"
)

type EmbedItem struct {
	Content message.Content `json:"content"`
}

func (i EmbedItem) Clone() EmbedItem {
	i.Content = i.Content.Clone()
	return i
}

func (i EmbedItem) Validate() error {
	return i.Content.Validate()
}

type EmbedRequest struct {
	Items      []EmbedItem `json:"items" ledger:"embed.items"`
	Dimensions *int        `json:"dimensions,omitempty" ledger:"embed.dimensions"`
	Extensions Extensions  `json:"-" ledger:"extension"`
}

func (r EmbedRequest) Clone() EmbedRequest {
	clone := r
	clone.Items = make([]EmbedItem, len(r.Items))
	for i, item := range r.Items {
		clone.Items[i] = item.Clone()
	}
	clone.Dimensions = clonePointer(r.Dimensions)
	clone.Extensions = r.Extensions.Clone()
	return clone
}

func (r EmbedRequest) ActiveFields() []FieldID {
	fields := activeTaggedFields(r)
	var hasText, hasImage, hasAudio, hasVideo, hasFile, hasData bool
	var hasToolCall, hasToolResult, hasMultiPart bool
	for _, item := range r.Items {
		hasMultiPart = hasMultiPart || len(item.Content.Parts) > 1
		for _, part := range item.Content.Parts {
			normalized, err := normalizePart(part)
			if err != nil {
				continue
			}
			switch normalized.(type) {
			case message.TextPart:
				hasText = true
			case message.ImagePart:
				hasImage = true
			case message.AudioPart:
				hasAudio = true
			case message.VideoPart:
				hasVideo = true
			case message.FilePart:
				hasFile = true
			case message.DataPart:
				hasData = true
			case message.ToolCallPart:
				hasToolCall = true
			case message.ToolResultPart:
				hasToolResult = true
			}
		}
	}
	if hasText {
		fields = append(fields, FieldEmbedItemText)
	}
	if hasImage {
		fields = append(fields, FieldEmbedItemImage)
	}
	if hasAudio {
		fields = append(fields, FieldEmbedItemAudio)
	}
	if hasVideo {
		fields = append(fields, FieldEmbedItemVideo)
	}
	if hasFile {
		fields = append(fields, FieldEmbedItemFile)
	}
	if hasData {
		fields = append(fields, FieldEmbedItemData)
	}
	if hasToolCall {
		fields = append(fields, FieldEmbedItemToolCall)
	}
	if hasToolResult {
		fields = append(fields, FieldEmbedItemToolResult)
	}
	if hasMultiPart {
		fields = append(fields, FieldEmbedItemMultiPart)
	}
	return r.Extensions.AppendActiveFields(fields)
}

func (r EmbedRequest) Validate() error {
	if len(r.Items) == 0 {
		return fmt.Errorf("embedding items are required")
	}
	for i, item := range r.Items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("embedding item %d: %w", i, err)
		}
	}
	if r.Dimensions != nil && *r.Dimensions <= 0 {
		return fmt.Errorf("embedding dimensions must be positive")
	}
	return r.Extensions.Validate()
}

type Embedding struct {
	Vector []float32 `json:"vector"`
}

type EmbedUsage struct {
	InputTokens int64 `json:"input_tokens,omitempty"`
	ItemCount   int   `json:"item_count"`
}

type EmbedResponse struct {
	Embeddings []Embedding `json:"embeddings"`
	Usage      EmbedUsage  `json:"usage"`
	Metadata   Metadata    `json:"metadata"`
}

func (r EmbedResponse) ValidateFor(request EmbedRequest) error {
	if len(r.Embeddings) != len(request.Items) {
		return fmt.Errorf("embedding count %d does not match item count %d", len(r.Embeddings), len(request.Items))
	}
	dimensions := 0
	for i, embedding := range r.Embeddings {
		if len(embedding.Vector) == 0 {
			return fmt.Errorf("embedding %d is empty", i)
		}
		if dimensions == 0 {
			dimensions = len(embedding.Vector)
		} else if len(embedding.Vector) != dimensions {
			return fmt.Errorf("embedding %d has inconsistent dimensions", i)
		}
		if request.Dimensions != nil && len(embedding.Vector) != *request.Dimensions {
			return fmt.Errorf("embedding %d does not match requested dimensions", i)
		}
		for _, value := range embedding.Vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return fmt.Errorf("embedding %d contains a non-finite value", i)
			}
		}
	}
	return nil
}
