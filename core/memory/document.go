package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/message"
)

// DocumentSink durably stores normalized document content and provenance. URI
// fetching, parsing, and media extraction happen before this boundary.
type DocumentSink interface {
	PutDocument(context.Context, Document) error
}

type Document struct {
	Scope          Scope
	DatasetID      string
	DocumentID     string
	IdempotencyKey string
	Content        message.Content
	Provenance     []SourceRef
	Metadata       Metadata
}

func (d Document) Validate() error {
	if err := d.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(d.DatasetID) == "" {
		return NewError(KindInvalidRequest, "document", errors.New("memory: dataset_id is required"))
	}
	if strings.TrimSpace(d.DocumentID) == "" {
		return NewError(KindInvalidRequest, "document", errors.New("memory: document_id is required"))
	}
	if strings.TrimSpace(d.IdempotencyKey) == "" {
		return NewError(KindInvalidRequest, "document", errors.New("memory: idempotency_key is required"))
	}
	if err := d.Content.Validate(); err != nil {
		return NewError(KindInvalidRequest, "document", fmt.Errorf("memory: document content: %w", err))
	}
	if len(d.Provenance) == 0 {
		return NewError(KindInvalidRequest, "document", errors.New("memory: document provenance is required"))
	}
	for index, source := range d.Provenance {
		if err := source.Validate(); err != nil {
			return NewError(KindInvalidRequest, "document", fmt.Errorf("memory: provenance %d: %w", index, err))
		}
	}
	return nil
}

func (d Document) Clone() Document {
	d.Content = d.Content.Clone()
	d.Provenance = append([]SourceRef(nil), d.Provenance...)
	d.Metadata = d.Metadata.Clone()
	return d
}
