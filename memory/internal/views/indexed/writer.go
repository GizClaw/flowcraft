package indexed

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	defaultEmbeddingBatchSize = 10
	maxEmbeddingInputRunes    = 8192
)

// Writer writes indexed projection records to the bound retrieval namespace.
type Writer struct {
	index            retrieval.Index
	binding          Binding
	embedder         embedding.Embedder
	vectorize        bool
	embeddingTimeout time.Duration
}

// WriterOption configures projection writer behavior.
type WriterOption func(*Writer)

// WithEmbedder installs the embedder used when vectorization is enabled.
func WithEmbedder(embedder embedding.Embedder) WriterOption {
	return func(w *Writer) {
		w.embedder = embedder
	}
}

// WithVectorize enables or disables filling missing record vectors from Text.
func WithVectorize(enabled bool) WriterOption {
	return func(w *Writer) {
		w.vectorize = enabled
	}
}

// WithEmbeddingTimeout bounds each EmbedBatch call when timeout is positive.
func WithEmbeddingTimeout(timeout time.Duration) WriterOption {
	return func(w *Writer) {
		w.embeddingTimeout = timeout
	}
}

// NewWriter validates the namespace binding and builds a retrieval index writer.
func NewWriter(index retrieval.Index, binding Binding, opts ...WriterOption) (*Writer, error) {
	if index == nil {
		return nil, errdefs.Validationf("%s: index is required", errPrefix)
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	writer := &Writer{
		index:   index,
		binding: cloneBinding(binding),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(writer)
		}
	}
	return writer, nil
}

// Binding returns a copy of the retrieval namespace binding.
func (w *Writer) Binding() Binding {
	return cloneBinding(w.binding)
}

// Upsert validates records, converts them to retrieval docs, and writes them to
// the bound namespace.
func (w *Writer) Upsert(ctx context.Context, records []Record) error {
	projected := append([]Record(nil), records...)
	if err := w.fillVectors(ctx, projected); err != nil {
		return err
	}
	docs := make([]retrieval.Doc, len(projected))
	for i, record := range projected {
		if err := record.Validate(); err != nil {
			return err
		}
		docs[i] = docFromRecord(record)
	}
	return w.index.Upsert(ctx, w.binding.Namespace, docs)
}

func (w *Writer) fillVectors(ctx context.Context, records []Record) error {
	if w == nil || !w.vectorize || w.embedder == nil || len(records) == 0 {
		return nil
	}
	texts := make([]string, 0, len(records))
	recordIndexes := make([]int, 0, len(records))
	for i, record := range records {
		if err := record.Validate(); err != nil {
			return err
		}
		if len(record.Vector) > 0 {
			continue
		}
		texts = append(texts, embeddingInputText(record.Text))
		recordIndexes = append(recordIndexes, i)
	}
	if len(texts) == 0 {
		return nil
	}
	embedCtx := ctx
	var cancel context.CancelFunc
	if w.embeddingTimeout > 0 {
		embedCtx, cancel = context.WithTimeout(ctx, w.embeddingTimeout)
		defer cancel()
	}
	for start := 0; start < len(texts); start += defaultEmbeddingBatchSize {
		end := min(start+defaultEmbeddingBatchSize, len(texts))
		vectors, err := w.embedder.EmbedBatch(embedCtx, texts[start:end])
		if err != nil {
			return fmt.Errorf("%s: embed projection records: %w", errPrefix, err)
		}
		if len(vectors) != end-start {
			return errdefs.Validationf("%s: embed projection records returned %d vectors for %d texts", errPrefix, len(vectors), end-start)
		}
		for i, vector := range vectors {
			records[recordIndexes[start+i]].Vector = append([]float32(nil), vector...)
		}
	}
	return nil
}

func embeddingInputText(text string) string {
	runes := []rune(text)
	if len(runes) <= maxEmbeddingInputRunes {
		return text
	}
	return string(runes[:maxEmbeddingInputRunes])
}

// Delete removes records by id from the bound namespace.
func (w *Writer) Delete(ctx context.Context, ids []string) error {
	return w.index.Delete(ctx, w.binding.Namespace, ids)
}

// Drop removes the bound namespace when the retrieval backend supports native
// namespace drops.
func (w *Writer) Drop(ctx context.Context) error {
	droppable, ok := w.index.(retrieval.Droppable)
	if !ok {
		return errdefs.NotAvailablef("%s: retrieval index does not support drop", errPrefix)
	}
	return droppable.Drop(ctx, w.binding.Namespace)
}

func docFromRecord(record Record) retrieval.Doc {
	return retrieval.Doc{
		ID:       record.ID,
		Content:  record.Text,
		Vector:   append([]float32(nil), record.Vector...),
		Metadata: metadataFromRecord(record),
	}
}

func metadataFromRecord(record Record) map[string]any {
	metadata := cloneMetadata(record.Metadata)
	if len(record.SourceRefs) == 0 && record.Signature.IsZero() {
		return metadata
	}
	if metadata == nil {
		metadata = make(map[string]any, 2)
	}
	if len(record.SourceRefs) > 0 {
		metadata[MetadataSourceRefsKey] = sourceRefsToDTO(record.SourceRefs)
	}
	if !record.Signature.IsZero() {
		metadata[MetadataSignatureKey] = signatureToDTO(record.Signature)
	}
	return metadata
}
