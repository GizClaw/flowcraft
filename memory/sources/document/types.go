package document

import "time"

// Document is a canonical evidence record for one raw external document.
type Document struct {
	DatasetID string
	// ID is the stable primary key within a dataset.
	ID string
	// Name is a display name, filename, or legacy alias. Migrations from an
	// older document source should explicitly set ID, often to the legacy Name.
	Name      string
	SourceURI string
	MIMEType  string
	Content   string
	// Metadata must be JSON-compatible. Values roundtrip through encoding/json,
	// so decoded maps use map[string]any, arrays use []any, and numbers use float64.
	Metadata    map[string]any
	Version     uint64
	ContentHash string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func cloneDocument(doc Document) Document {
	if doc.Metadata != nil {
		doc.Metadata = cloneAnyMap(doc.Metadata)
	}
	return doc
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return cloneAnyMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = cloneAny(item)
		}
		return out
	default:
		return v
	}
}
