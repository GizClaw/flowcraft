package inference

import (
	"fmt"
	"math"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
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
	clone.Dimensions = model.ClonePointer(r.Dimensions)
	clone.Extensions = r.Extensions.Clone()
	return clone
}

func (r EmbedRequest) ActiveFields() []FieldID {
	var fields []FieldID
	if len(r.Items) > 0 {
		fields = append(fields, FieldEmbedItems)
	}
	if r.Dimensions != nil {
		fields = append(fields, FieldEmbedDimensions)
	}
	kinds := make(map[message.PartKind]bool)
	multiPart := false
	for _, item := range r.Items {
		multiPart = multiPart || len(item.Content.Parts) > 1
		for _, part := range item.Content.Parts {
			normalized, err := message.NormalizePart(part)
			if err != nil {
				continue
			}
			kinds[normalized.Kind()] = true
		}
	}
	fields = appendPartFields(fields, embedItemPartFields, kinds)
	if multiPart {
		fields = append(fields, FieldEmbedItemMultiPart)
	}
	return r.Extensions.AppendActiveFields(fields)
}

// EmbedItemPartField returns the ledger field one embedding item activates for
// one content kind. Reasoning has no row: it is not an embedding input, so the
// boolean is false for it and for any kind outside the vocabulary — see
// GenerateContextPartField for what false means.
func EmbedItemPartField(kind message.PartKind) (FieldID, bool) {
	field, ok := embedItemPartFields[kind]
	return field, ok
}

// embedItemPartFields maps a content kind onto the ledger field an embedding
// item activates. Reasoning has no row: it is a trace of the model's own
// process, not an input an embedding call can consume.
// TestEmbedLedgerCoversPartKinds pins the table against the vocabulary.
var embedItemPartFields = map[message.PartKind]FieldID{
	message.PartText:       FieldEmbedItemText,
	message.PartImage:      FieldEmbedItemImage,
	message.PartAudio:      FieldEmbedItemAudio,
	message.PartVideo:      FieldEmbedItemVideo,
	message.PartFile:       FieldEmbedItemFile,
	message.PartData:       FieldEmbedItemData,
	message.PartToolCall:   FieldEmbedItemToolCall,
	message.PartToolResult: FieldEmbedItemToolResult,
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
