package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/core/message"
)

// MaxDocumentRunes bounds the text one document carries.
//
// A document is materialized as runes before it is derived -- joined, split
// into sections, cut into chunks, and every section and chunk becomes an item
// the build publishes -- so what a document costs is its text, not its
// envelope. An input nobody bounded is therefore a build nobody can finish:
// this ceiling is what makes "derive this document" a bounded amount of work
// for one process, and it is generous for the artifact it names (a book's
// worth of prose, or several hundred pages of it in a script where a rune is
// most of a byte).
//
// It bounds the text, which is the part that is amplified. Media that rides
// along in the document is stored as provenance and never derived, so it is
// not part of the build; a service that accepts uploads bounds those at the
// edge, where the payload exists once instead of once per copy this path makes.
const MaxDocumentRunes = 1_000_000

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
	if runes := DocumentTextRunes(d.Content); runes > MaxDocumentRunes {
		return NewError(KindInvalidRequest, "document", fmt.Errorf(
			"memory: document text is %d runes, above the %d-rune document ceiling",
			runes, MaxDocumentRunes))
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

// DocumentTextRunes reports the runes of text content carries without joining
// the parts, so measuring a document does not allocate the string it would
// measure. Only text is counted: it is what a deriver reads, and a part it
// does not read is not part of the build.
func DocumentTextRunes(content message.Content) int {
	runes := 0
	for _, part := range content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		if text, ok := normalized.(message.TextPart); ok {
			runes += utf8.RuneCountInString(text.Text)
		}
	}
	return runes
}

func (d Document) Clone() Document {
	d.Content = d.Content.Clone()
	d.Provenance = append([]SourceRef(nil), d.Provenance...)
	d.Metadata = d.Metadata.Clone()
	return d
}
