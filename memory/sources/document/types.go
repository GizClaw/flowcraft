package document

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

// Document is the canonical value for one stable document primary key.
type Document struct {
	Scope      sdkmemory.Scope       `json:"scope"`
	DatasetID  string                `json:"dataset_id"`
	DocumentID string                `json:"document_id"`
	Content    sdkmessage.Content    `json:"content"`
	Provenance []sdkmemory.SourceRef `json:"provenance"`
	Metadata   sdkmemory.Metadata    `json:"metadata,omitempty"`
	Version    uint64                `json:"version"`
	CreatedAt  time.Time             `json:"created_at"`
	UpdatedAt  time.Time             `json:"updated_at"`
}

type Operation string

const (
	OperationPut       Operation = "put"
	OperationTombstone Operation = "tombstone"
)

// Event is one immutable source revision and durable derivation work item.
type Event struct {
	ID         string                `json:"id"`
	Operation  Operation             `json:"operation"`
	Scope      sdkmemory.Scope       `json:"scope"`
	DatasetID  string                `json:"dataset_id"`
	DocumentID string                `json:"document_id"`
	Version    uint64                `json:"version"`
	OutboxSeq  uint64                `json:"outbox_seq,omitempty"`
	Document   *Document             `json:"document,omitempty"`
	Provenance []sdkmemory.SourceRef `json:"provenance"`
	CreatedAt  time.Time             `json:"created_at"`
}

// PutRequest stores or replaces one document. IdempotencyKey is scoped to
// Scope's hard partition, DatasetID, and DocumentID.
type PutRequest struct {
	Scope          sdkmemory.Scope
	DatasetID      string
	DocumentID     string
	IdempotencyKey string
	Content        sdkmessage.Content
	Provenance     []sdkmemory.SourceRef
	Metadata       sdkmemory.Metadata
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
	document.Provenance = append([]sdkmemory.SourceRef(nil), document.Provenance...)
	document.Metadata = document.Metadata.Clone()
	return document
}

func cloneEvent(event Event) Event {
	if event.Document != nil {
		document := cloneDocument(*event.Document)
		event.Document = &document
	}
	event.Provenance = append([]sdkmemory.SourceRef(nil), event.Provenance...)
	return event
}

func eventID(scope sdkmemory.Scope, datasetID, documentID string, version uint64, operation Operation) string {
	sum := sha256.Sum256([]byte(scope.HardPartitionKey() + "\x00" + datasetID + "\x00" + documentID +
		"\x00" + strconv.FormatUint(version, 10) + "\x00" + string(operation)))
	return "document-event-" + hex.EncodeToString(sum[:])
}
