package views

import (
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// SourceKind identifies a canonical evidence source family.
type SourceKind string

const (
	SourceMessage  SourceKind = "message"
	SourceDocument SourceKind = "document"
)

// Span identifies an inclusive/exclusive offset range within source content.
type Span struct {
	Start int
	End   int
}

// Validate checks that the span is empty or points at a non-negative range.
func (s Span) Validate() error {
	if s.Start < 0 {
		return errdefs.Validationf("memory/views: span start must be non-negative")
	}
	if s.End < s.Start {
		return errdefs.Validationf("memory/views: span end must be greater than or equal to start")
	}
	return nil
}

// SourceRef identifies one piece of canonical evidence used by a derived view.
// StableKey only uses stable evidence identity plus optional span; source-side
// freshness identity belongs in SourceRevision. It does not describe
// dependencies on other derived views; use UpstreamViewRef in ViewSignature for
// view-to-view lineage.
type SourceRef struct {
	Kind     SourceKind
	Message  *MessageSourceRef
	Document *DocumentSourceRef
}

// EvidenceRef is an alias for SourceRef for call sites that read more naturally
// in terms of evidence lineage.
type EvidenceRef = SourceRef

// MessageSourceRef points at a canonical conversation message.
type MessageSourceRef struct {
	ConversationID string
	MessageID      string
	Span           *Span
}

// DocumentSourceRef points at a canonical external document.
// DatasetID, DocumentID, and optional Span are the stable evidence identity.
// Version and ContentHash are optional citation-grounding metadata; they do not
// participate in StableKey or freshness identity. SourceRevision carries
// source-side freshness.
type DocumentSourceRef struct {
	DatasetID   string
	DocumentID  string
	Version     string
	ContentHash string
	Span        *Span
}

// Validate checks source kind, payload shape, required identity fields, and span
// bounds. Exactly one payload must be present and it must match Kind.
func (r SourceRef) Validate() error {
	if !validSourceKind(r.Kind) {
		return errdefs.Validationf("memory/views: unsupported source kind %q", r.Kind)
	}

	payloads := 0
	if r.Message != nil {
		payloads++
	}
	if r.Document != nil {
		payloads++
	}
	if payloads != 1 {
		return errdefs.Validationf("memory/views: source ref requires exactly one payload")
	}

	switch r.Kind {
	case SourceMessage:
		if r.Message == nil {
			return errdefs.Validationf("memory/views: message source ref requires message payload")
		}
		if r.Document != nil {
			return errdefs.Validationf("memory/views: message source ref must not include document payload")
		}
		return r.Message.Validate()
	case SourceDocument:
		if r.Document == nil {
			return errdefs.Validationf("memory/views: document source ref requires document payload")
		}
		if r.Message != nil {
			return errdefs.Validationf("memory/views: document source ref must not include message payload")
		}
		return r.Document.Validate()
	default:
		return errdefs.Validationf("memory/views: unsupported source kind %q", r.Kind)
	}
}

// StableKey returns a deterministic stable source/span identity key for a valid
// reference. Invalid refs panic; use StableKeyE when callers need an error.
//
// The encoded schema is private and fixed so public struct field names can
// evolve without changing long-lived source identity. JSON keeps field
// boundaries explicit, avoiding collisions when IDs contain punctuation.
func (r SourceRef) StableKey() string {
	key, err := r.StableKeyE()
	if err != nil {
		panic(err)
	}
	return key
}

// StableKeyE returns a deterministic stable source/span identity key for a valid
// reference. Revision and content hash are intentionally excluded from this key;
// SourceRevision carries source-side freshness identity.
func (r SourceRef) StableKeyE() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	key := newSourceRefStableKey(r)
	encoded, err := json.Marshal(key)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// Validate checks required message identity fields and span bounds.
func (r MessageSourceRef) Validate() error {
	if r.ConversationID == "" {
		return errdefs.Validationf("memory/views: message source ref conversation_id is required")
	}
	if r.MessageID == "" {
		return errdefs.Validationf("memory/views: message source ref message_id is required")
	}
	if r.Span != nil {
		return r.Span.Validate()
	}
	return nil
}

// Validate checks required document identity fields and span bounds.
func (r DocumentSourceRef) Validate() error {
	if r.DatasetID == "" {
		return errdefs.Validationf("memory/views: document source ref dataset_id is required")
	}
	if r.DocumentID == "" {
		return errdefs.Validationf("memory/views: document source ref document_id is required")
	}
	if r.Span != nil {
		return r.Span.Validate()
	}
	return nil
}

func validSourceKind(kind SourceKind) bool {
	switch kind {
	case SourceMessage, SourceDocument:
		return true
	default:
		return false
	}
}

const sourceRefStableKeySchema = "views.source_ref.v1"

type sourceRefStableKey struct {
	Schema   string                      `json:"schema"`
	Kind     SourceKind                  `json:"kind"`
	Message  *messageSourceRefStableKey  `json:"message,omitempty"`
	Document *documentSourceRefStableKey `json:"document,omitempty"`
}

type messageSourceRefStableKey struct {
	ConversationID string         `json:"conversation_id"`
	MessageID      string         `json:"message_id"`
	Span           *spanStableKey `json:"span,omitempty"`
}

type documentSourceRefStableKey struct {
	DatasetID  string         `json:"dataset_id"`
	DocumentID string         `json:"document_id"`
	Span       *spanStableKey `json:"span,omitempty"`
}

type spanStableKey struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

func newSourceRefStableKey(r SourceRef) sourceRefStableKey {
	key := sourceRefStableKey{
		Schema: sourceRefStableKeySchema,
		Kind:   r.Kind,
	}
	if r.Message != nil {
		key.Message = &messageSourceRefStableKey{
			ConversationID: r.Message.ConversationID,
			MessageID:      r.Message.MessageID,
			Span:           newSpanStableKey(r.Message.Span),
		}
	}
	if r.Document != nil {
		key.Document = &documentSourceRefStableKey{
			DatasetID:  r.Document.DatasetID,
			DocumentID: r.Document.DocumentID,
			Span:       newSpanStableKey(r.Document.Span),
		}
	}
	return key
}

func newSpanStableKey(span *Span) *spanStableKey {
	if span == nil {
		return nil
	}
	return &spanStableKey{
		Start: span.Start,
		End:   span.End,
	}
}
