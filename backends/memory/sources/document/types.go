package document

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// Document is the canonical value for one stable document primary key.
type Document struct {
	Scope      corememory.Scope       `json:"scope"`
	DatasetID  string                 `json:"dataset_id"`
	DocumentID string                 `json:"document_id"`
	Content    coremessage.Content    `json:"content"`
	Provenance []corememory.SourceRef `json:"provenance"`
	Metadata   corememory.Metadata    `json:"metadata,omitempty"`
	Version    uint64                 `json:"version"`
	CreatedAt  time.Time              `json:"created_at"`
	UpdatedAt  time.Time              `json:"updated_at"`
}

type Operation string

const (
	OperationPut       Operation = "put"
	OperationTombstone Operation = "tombstone"
)

// Event is one immutable source revision and durable derivation work item.
type Event struct {
	ID         string                 `json:"id"`
	Operation  Operation              `json:"operation"`
	Scope      corememory.Scope       `json:"scope"`
	DatasetID  string                 `json:"dataset_id"`
	DocumentID string                 `json:"document_id"`
	Version    uint64                 `json:"version"`
	OutboxSeq  uint64                 `json:"outbox_seq,omitempty"`
	Document   *Document              `json:"document,omitempty"`
	Provenance []corememory.SourceRef `json:"provenance"`
	CreatedAt  time.Time              `json:"created_at"`
}

// PutRequest stores or replaces one document. IdempotencyKey is scoped to
// Scope's hard partition, DatasetID, and DocumentID.
type PutRequest struct {
	Scope          corememory.Scope
	DatasetID      string
	DocumentID     string
	IdempotencyKey string
	Content        coremessage.Content
	Provenance     []corememory.SourceRef
	Metadata       corememory.Metadata
}

// ListOptions selects documents whose IDs are lexically greater than AfterID.
// A non-positive Limit means no limit.
type ListOptions struct {
	AfterID string
	Limit   int
}

// ListEventOptions selects source events strictly after the scope-wide
// AfterOutboxSeq cursor.
type ListEventOptions struct {
	AfterOutboxSeq uint64
	Limit          int
}

// ListDocumentEventOptions selects one document's revisions strictly after
// AfterVersion.
type ListDocumentEventOptions struct {
	AfterVersion uint64
	Limit        int
}

func cloneDocument(document Document) Document {
	document.Content = document.Content.Clone()
	document.Provenance = append([]corememory.SourceRef(nil), document.Provenance...)
	document.Metadata = document.Metadata.Clone()
	return document
}

func cloneEvent(event Event) Event {
	if event.Document != nil {
		document := cloneDocument(*event.Document)
		event.Document = &document
	}
	event.Provenance = append([]corememory.SourceRef(nil), event.Provenance...)
	return event
}

func eventID(scope corememory.Scope, datasetID, documentID string, version uint64, operation Operation) string {
	sum := sha256.Sum256([]byte(scope.HardPartitionKey() + "\x00" + datasetID + "\x00" + documentID +
		"\x00" + strconv.FormatUint(version, 10) + "\x00" + string(operation)))
	return "document-event-" + hex.EncodeToString(sum[:])
}
