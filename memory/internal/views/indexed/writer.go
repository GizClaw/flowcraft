package indexed

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Writer writes indexed projection records to the bound retrieval namespace.
type Writer struct {
	index   retrieval.Index
	binding Binding
}

// NewWriter validates the namespace binding and builds a retrieval index writer.
func NewWriter(index retrieval.Index, binding Binding) (*Writer, error) {
	if index == nil {
		return nil, errdefs.Validationf("%s: index is required", errPrefix)
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	return &Writer{
		index:   index,
		binding: cloneBinding(binding),
	}, nil
}

// Binding returns a copy of the retrieval namespace binding.
func (w *Writer) Binding() Binding {
	return cloneBinding(w.binding)
}

// Upsert validates records, converts them to retrieval docs, and writes them to
// the bound namespace.
func (w *Writer) Upsert(ctx context.Context, records []Record) error {
	docs := make([]retrieval.Doc, len(records))
	for i, record := range records {
		if err := record.Validate(); err != nil {
			return err
		}
		docs[i] = docFromRecord(record)
	}
	return w.index.Upsert(ctx, w.binding.Namespace, docs)
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
