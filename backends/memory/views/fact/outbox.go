package fact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	"github.com/GizClaw/flowcraft/core/errdefs"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// publicationEventType names one outbox record in the scope-wide stream.
const publicationEventType = "fact.publication"

// PublicationRecord is one durable outbox entry: the publication it names,
// plus the scope and conversation a consumer has to look at. It does not copy
// the fact -- the fact is durable in the view before the record is written,
// and a copy could disagree with it.
type PublicationRecord struct {
	SchemaVersion  int    `json:"schema_version"`
	RuntimeID      string `json:"runtime_id"`
	UserID         string `json:"user_id"`
	AgentID        string `json:"agent_id,omitempty"`
	ConversationID string `json:"conversation_id"`
	FactID         string `json:"fact_id"`
	Event          string `json:"event"`
	RevisionDigest string `json:"revision_digest"`
	PublicationID  string `json:"publication_id"`
}

// Scope returns the scope the record was written under.
func (record PublicationRecord) Scope() corememory.Scope {
	return corememory.Scope{RuntimeID: record.RuntimeID, UserID: record.UserID, AgentID: record.AgentID}
}

// Outbox is the durable write side of the publication outbox: every
// publication the view emits is appended to a scope-wide log stream, keyed by
// the publication ID so that a retry of the same logical event lands once.
//
// It is only the write half. Nothing consumes the stream yet: the polling
// path (ListPublications) is still what a consumer can reconcile from, and a
// record only ever adds a trigger -- it never carries authority the view does
// not already have.
type Outbox struct {
	log storage.Log
}

// NewOutbox returns the outbox over a log substrate. The log has to be the
// one the view's own events are durable in: a record that survived a crash
// while its fact did not would trigger a derivation of nothing.
func NewOutbox(log storage.Log) (*Outbox, error) {
	if nilValue(log) {
		return nil, errors.New("fact view: outbox log is required")
	}
	return &Outbox{log: log}, nil
}

// PublishFact implements PublicationSink.
func (outbox *Outbox) PublishFact(ctx context.Context, publication Publication) error {
	if outbox == nil || nilValue(outbox.log) {
		return errors.New("fact view: outbox is required")
	}
	record, err := recordFor(publication)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("fact view: encode publication %q: %w", publication.PublicationID, err)
	}
	stream, err := publicationsStream(publication.Fact.Scope)
	if err != nil {
		return err
	}
	if _, err := outbox.log.Append(ctx, stream, []storage.Event{{
		Stream:  stream,
		Type:    publicationEventType,
		Payload: payload,
	}}, storage.AppendOptions{IdempotencyKey: publication.PublicationID}); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrConflict) {
		return fmt.Errorf("fact view: write publication %q: %w", publication.PublicationID, err)
	}
	// The key is in use. A retry of the same publication is the ordinary
	// case and has to come back clean; anything else is a collision the
	// caller has to see rather than a duplicate the outbox hides.
	records, _, err := outbox.Read(ctx, publication.Fact.Scope, 0, 0)
	if err != nil {
		return err
	}
	for _, prior := range records {
		if prior.PublicationID != publication.PublicationID {
			continue
		}
		if prior == record {
			return nil
		}
		return errdefs.Conflictf(
			"fact view: publication %q is already in the outbox with a different record", publication.PublicationID)
	}
	return errdefs.Conflictf("fact view: publication id %q is taken by another record", publication.PublicationID)
}

// Read returns the outbox records of one scope in stream order, after the
// given log sequence, together with the sequence of the last record read. A
// zero after reads from the beginning; a non-positive limit reads everything.
// The sequence is the cursor a consumer resumes from, and the stream is the
// reason records are append-only: a record is never rewritten, so a cursor
// cannot skip one.
func (outbox *Outbox) Read(
	ctx context.Context,
	scope corememory.Scope,
	after uint64,
	limit int,
) ([]PublicationRecord, uint64, error) {
	entries, cursor, err := outbox.ReadEntries(ctx, scope, after, limit)
	if err != nil {
		return nil, 0, err
	}
	records := make([]PublicationRecord, 0, len(entries))
	for _, entry := range entries {
		records = append(records, entry.Record)
	}
	return records, cursor, nil
}

// OutboxEntry is one outbox record together with the log sequence it is stored
// at. Read hands back the records alone; a consumer that has to leave a record
// for a later poll -- because the work it names is not done yet -- needs the
// sequence, and that is what this is for.
type OutboxEntry struct {
	Seq    uint64
	Record PublicationRecord
}

// ReadEntries returns the outbox entries of one scope in stream order, after
// the given log sequence, together with the sequence of the last entry read. A
// zero after reads from the beginning; a non-positive limit reads everything.
// The sequence is the cursor a consumer resumes from, and the stream is the
// reason records are append-only: a record is never rewritten, so a cursor
// cannot skip one.
func (outbox *Outbox) ReadEntries(
	ctx context.Context,
	scope corememory.Scope,
	after uint64,
	limit int,
) ([]OutboxEntry, uint64, error) {
	if outbox == nil || nilValue(outbox.log) {
		return nil, 0, errors.New("fact view: outbox is required")
	}
	stream, err := publicationsStream(scope)
	if err != nil {
		return nil, 0, err
	}
	events, err := outbox.log.Read(ctx, stream, after, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("fact view: read outbox: %w", err)
	}
	entries := make([]OutboxEntry, 0, len(events))
	cursor := after
	for _, event := range events {
		var record PublicationRecord
		if err := decodeJSON(event.Payload, &record); err != nil {
			return nil, 0, fmt.Errorf("fact view: decode outbox record %d: %w", event.Seq, err)
		}
		if err := validateRecord(record); err != nil {
			return nil, 0, fmt.Errorf("fact view: corrupt outbox record %d: %w", event.Seq, err)
		}
		entries = append(entries, OutboxEntry{Seq: event.Seq, Record: record})
		cursor = event.Seq
	}
	return entries, cursor, nil
}

// publicationsStream names the scope-wide outbox stream, next to the view's
// other keys in its namespace.
func publicationsStream(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return rootPrefix + "/" + partition + "/publications", nil
}

// recordFor turns a publication into the record that outlives it. The
// publication ID is the record's idempotency key, so a record that is not
// identifiable does not belong in the outbox.
func recordFor(publication Publication) (PublicationRecord, error) {
	scope := publication.Fact.Scope
	if err := scope.Validate(); err != nil {
		return PublicationRecord{}, err
	}
	record := PublicationRecord{
		SchemaVersion:  schemaVersion,
		RuntimeID:      scope.RuntimeID,
		UserID:         scope.UserID,
		AgentID:        scope.AgentID,
		ConversationID: publication.Fact.ConversationID,
		FactID:         publication.Fact.ID,
		Event:          publication.Event,
		RevisionDigest: publication.RevisionDigest,
		PublicationID:  publication.PublicationID,
	}
	if err := validateRecord(record); err != nil {
		return PublicationRecord{}, err
	}
	return record, nil
}

func validateRecord(record PublicationRecord) error {
	if err := record.Scope().Validate(); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(record.ConversationID) == "":
		return errors.New("fact view: outbox record has no conversation id")
	case strings.TrimSpace(record.FactID) == "":
		return errors.New("fact view: outbox record has no fact id")
	case strings.TrimSpace(record.Event) == "":
		return errors.New("fact view: outbox record has no event")
	case strings.TrimSpace(record.RevisionDigest) == "":
		return errors.New("fact view: outbox record has no revision digest")
	case strings.TrimSpace(record.PublicationID) == "":
		return errors.New("fact view: outbox record has no publication id")
	}
	return nil
}
