package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const (
	triggerRoot      = "worker/v1/triggers"
	triggerEventType = "derivation.trigger"
	triggerSchema    = 1
	// defaultTriggerBatch bounds how many records one poll reads from one
	// source. The records are triggers, not payloads: a bound this side of the
	// stream keeps one poll's read from following how long a runner was away.
	defaultTriggerBatch = 256
	// defaultLeaseTTL bounds one scope's lease. It is longer than the poll
	// period a runner is built with, so an idle scope is not fought over, and a
	// runner renews it while it derives.
	defaultLeaseTTL = 30 * time.Second

	// TriggerKindTurn names a record for a conversation whose messages were
	// committed: the write path asking for the turn to become retrievable.
	TriggerKindTurn = "turn"
	// TriggerKindDocument names a record for a scope whose document stream
	// advanced. Documents are scope-scoped, not conversation-scoped: their
	// derivation is one pass over the scope's document outbox.
	TriggerKindDocument = "document"
	// TriggerKindPublication names a record for a conversation a fact was
	// published for. It is the fact view's own signal: a fact that arrived
	// through the view -- an import, a backfill, a host writing facts directly
	// -- becomes retrievable when a pass indexes it.
	TriggerKindPublication = "publication"
)

// TriggerRecord is one durable work record: the name of something that has
// happened and whose derivation is owed. It carries no payload -- the commit,
// the document and the fact are durable where they belong before the record is
// written -- so a consumer resolves the work itself, and a record that is read
// twice asks for work that is already done.
type TriggerRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	RuntimeID     string `json:"runtime_id"`
	UserID        string `json:"user_id"`
	AgentID       string `json:"agent_id,omitempty"`
	// ConversationID names the conversation a turn or a publication belongs
	// to. A document record has none.
	ConversationID string `json:"conversation_id,omitempty"`
	// Ref identifies the durable item the record names within its kind: a
	// commit id, a document version, or a publication id. Together with the
	// kind it is the record's identity, which is what makes a replayed record
	// land once.
	Ref string `json:"ref"`
}

// Scope returns the scope the record was written under.
func (record TriggerRecord) Scope() corememory.Scope {
	return corememory.Scope{RuntimeID: record.RuntimeID, UserID: record.UserID, AgentID: record.AgentID}
}

// TriggerEntry is one journal record together with the log sequence it is
// stored at: the cursor a consumer resumes from.
type TriggerEntry struct {
	Seq    uint64
	Record TriggerRecord
}

// TriggerJournal is the scope-level work journal behind the trigger: an
// append-only stream of work records, one per derivation a write path owes.
//
// It exists because the sweep is the only thing that could notice work until
// now, and the sweep runs on a period. A write path that records its own work
// lets a runner derive the conversation it named within a poll, while the
// sweep stays what it was: the fallback that derives everything the journal
// never saw -- an importer writing to the message store directly, a record that
// failed to land, a workspace another version of this module wrote.
//
// The journal is a queue, not a log of authority: a record names work, and the
// work is idempotent. Losing one costs latency, never correctness, which is why
// a write path may report its turn committed even when the record did not land.
type TriggerJournal struct {
	log storage.Log
}

// TriggerJournalOption configures the journal.
type TriggerJournalOption func(*TriggerJournal)

// NewTriggerJournal constructs the journal over a log substrate.
func NewTriggerJournal(log storage.Log, options ...TriggerJournalOption) (*TriggerJournal, error) {
	if nilInterface(log) {
		return nil, errors.New("memory worker: trigger journal log is required")
	}
	journal := &TriggerJournal{log: log}
	for _, option := range options {
		if option != nil {
			option(journal)
		}
	}
	return journal, nil
}

// RecordTurn appends one record for a committed turn. The reference is the
// commit, so a retried commit replays as one record.
func (journal *TriggerJournal) RecordTurn(
	ctx context.Context,
	scope corememory.Scope,
	conversationID, commitID string,
) error {
	return journal.Record(ctx, scope, TriggerRecord{
		Kind: TriggerKindTurn, ConversationID: conversationID, Ref: "commit:" + commitID,
	})
}

// RecordDocument appends one record for a scope whose document stream advanced.
// The reference identifies the document version, so re-putting one document
// replays as one record.
func (journal *TriggerJournal) RecordDocument(
	ctx context.Context,
	scope corememory.Scope,
	ref string,
) error {
	return journal.Record(ctx, scope, TriggerRecord{Kind: TriggerKindDocument, Ref: ref})
}

// Read returns the journal entries of one scope in stream order, after the
// given log sequence, together with the sequence of the last entry read. A zero
// after reads from the beginning; a non-positive limit reads everything.
func (journal *TriggerJournal) Read(
	ctx context.Context,
	scope corememory.Scope,
	after uint64,
	limit int,
) ([]TriggerEntry, uint64, error) {
	if journal == nil || nilInterface(journal.log) {
		return nil, 0, errors.New("memory worker: trigger journal is required")
	}
	if ctx == nil {
		return nil, 0, errors.New("memory worker: context is required")
	}
	stream, err := triggerJournalStream(scope)
	if err != nil {
		return nil, 0, err
	}
	events, err := journal.log.Read(ctx, stream, after, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("memory worker: read trigger journal: %w", err)
	}
	entries := make([]TriggerEntry, 0, len(events))
	cursor := after
	for _, event := range events {
		var record TriggerRecord
		if err := decodeStrict(event.Payload, &record); err != nil {
			return nil, 0, fmt.Errorf("memory worker: decode trigger record %d: %w", event.Seq, err)
		}
		if err := validateTriggerRecord(record); err != nil {
			return nil, 0, fmt.Errorf("memory worker: corrupt trigger record %d: %w", event.Seq, err)
		}
		entries = append(entries, TriggerEntry{Seq: event.Seq, Record: record})
		cursor = event.Seq
	}
	return entries, cursor, nil
}

// Record appends one work record under its own identity. A record whose key is
// already in the journal is the ordinary case of a write path retrying: the
// same record comes back clean, and a different record under the same key is a
// conflict the caller has to see.
func (journal *TriggerJournal) Record(
	ctx context.Context,
	scope corememory.Scope,
	record TriggerRecord,
) error {
	if journal == nil || nilInterface(journal.log) {
		return errors.New("memory worker: trigger journal is required")
	}
	if ctx == nil {
		return errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	record.SchemaVersion = triggerSchema
	record.RuntimeID, record.UserID, record.AgentID = scope.RuntimeID, scope.UserID, scope.AgentID
	if err := validateTriggerRecord(record); err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("memory worker: encode trigger record: %w", err)
	}
	stream, err := triggerJournalStream(scope)
	if err != nil {
		return err
	}
	key := record.Kind + ":" + record.Ref
	if _, err := journal.log.Append(ctx, stream, []storage.Event{{
		Stream: stream, Type: triggerEventType, Payload: payload,
	}}, storage.AppendOptions{IdempotencyKey: key}); err != nil {
		return fmt.Errorf("memory worker: append trigger record: %w", err)
	}
	return nil
}

func triggerJournalStream(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return triggerRoot + "/" + partition + "/journal", nil
}

func validateTriggerRecord(record TriggerRecord) error {
	if err := record.Scope().Validate(); err != nil {
		return err
	}
	switch record.Kind {
	case TriggerKindTurn, TriggerKindPublication:
		if strings.TrimSpace(record.ConversationID) == "" {
			return fmt.Errorf("memory worker: %s record has no conversation id", record.Kind)
		}
	case TriggerKindDocument:
		if strings.TrimSpace(record.ConversationID) != "" {
			return errors.New("memory worker: document record names a conversation")
		}
	default:
		return fmt.Errorf("memory worker: trigger record kind %q is not supported", record.Kind)
	}
	if strings.TrimSpace(record.Ref) == "" {
		return errors.New("memory worker: trigger record has no reference")
	}
	return nil
}

// TriggerCursor is one scope's consumer position in one source: the log
// sequence the trigger has consumed up to. It is durable because the work
// behind a record is durable: a runner that restarts resumes from the
// conversations it had not derived, not from the beginning of the stream.
type TriggerCursor struct {
	Scope     corememory.Scope `json:"scope"`
	Source    string           `json:"source"`
	Seq       uint64           `json:"seq"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// TriggerStore persists one scope's consumer position per source.
type TriggerStore struct {
	kv    storage.Store
	clock func() time.Time
}

// TriggerStoreOption configures the cursor store.
type TriggerStoreOption func(*TriggerStore)

// WithTriggerClock replaces the clock the cursor timestamps are written with.
func WithTriggerClock(clock func() time.Time) TriggerStoreOption {
	return func(store *TriggerStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// NewTriggerStore constructs a cursor store over a KV substrate.
func NewTriggerStore(kv storage.Store, options ...TriggerStoreOption) (*TriggerStore, error) {
	if nilInterface(kv) {
		return nil, errors.New("memory worker: trigger kv store is required")
	}
	store := &TriggerStore{kv: kv, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store, nil
}

// LoadCursor returns the stored consumer position of one scope and source. A
// position that was never stored reads as the beginning of the stream.
func (store *TriggerStore) LoadCursor(
	ctx context.Context,
	scope corememory.Scope,
	source string,
) (TriggerCursor, bool, error) {
	if store == nil || nilInterface(store.kv) {
		return TriggerCursor{}, false, errors.New("memory worker: trigger store is incomplete")
	}
	if ctx == nil {
		return TriggerCursor{}, false, errors.New("memory worker: context is required")
	}
	key, err := triggerCursorKey(scope, source)
	if err != nil {
		return TriggerCursor{}, false, err
	}
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return TriggerCursor{}, false, nil
		}
		return TriggerCursor{}, false, fmt.Errorf("memory worker: read trigger cursor: %w", err)
	}
	var cursor TriggerCursor
	if err := decodeStrict(data, &cursor); err != nil {
		return TriggerCursor{}, false, fmt.Errorf("memory worker: decode trigger cursor: %w", err)
	}
	if err := validateTriggerCursor(cursor, key); err != nil {
		return TriggerCursor{}, false, err
	}
	return cursor, true, nil
}

// SaveCursor publishes one consumer position, overwriting the previous value.
func (store *TriggerStore) SaveCursor(ctx context.Context, cursor TriggerCursor) error {
	if store == nil || nilInterface(store.kv) {
		return errors.New("memory worker: trigger store is incomplete")
	}
	if ctx == nil {
		return errors.New("memory worker: context is required")
	}
	if cursor.UpdatedAt.IsZero() {
		cursor.UpdatedAt = store.clock().UTC()
	}
	key, err := triggerCursorKey(cursor.Scope, cursor.Source)
	if err != nil {
		return err
	}
	if err := validateTriggerCursor(cursor, key); err != nil {
		return err
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return fmt.Errorf("memory worker: encode trigger cursor: %w", err)
	}
	if err := store.kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("memory worker: write trigger cursor: %w", err)
	}
	return nil
}

func triggerCursorKey(scope corememory.Scope, source string) (string, error) {
	if strings.TrimSpace(source) == "" {
		return "", errors.New("memory worker: trigger source is required")
	}
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return triggerRoot + "/" + partition + "/cursors/" + storage.EncodeSegment(source), nil
}

func validateTriggerCursor(cursor TriggerCursor, key string) error {
	if err := cursor.Scope.Validate(); err != nil {
		return fmt.Errorf("memory worker: trigger cursor %q: %w", key, err)
	}
	expected, err := triggerCursorKey(cursor.Scope, cursor.Source)
	if err != nil {
		return err
	}
	if expected != key {
		return errors.New("memory worker: persisted trigger cursor does not match its key")
	}
	return nil
}

// TriggerStats is a snapshot of one trigger's cumulative counters.
type TriggerStats struct {
	// Runs counts the scopes the polls have drained, split by what the drain
	// found: ScopesLeased is the scopes it took, ScopesBusy the scopes another
	// runner held, ScopesRepairing the scopes whose lanes are being rebuilt.
	Runs            int64 `json:"runs"`
	ScopesLeased    int64 `json:"scopes_leased"`
	ScopesBusy      int64 `json:"scopes_busy"`
	ScopesRepairing int64 `json:"scopes_repairing"`
	// RecordsRead counts the records the polls have read, Consumed the ones
	// they advanced a cursor past. A record read but not consumed is work the
	// next poll has to look at again.
	RecordsRead   int64     `json:"records_read"`
	Consumed      int64     `json:"consumed"`
	Conversations int64     `json:"conversations"`
	Derived       int64     `json:"derived"`
	Retried       int64     `json:"retried"`
	Documents     int64     `json:"documents"`
	LeasesLost    int64     `json:"leases_lost"`
	LastError     string    `json:"last_error,omitempty"`
	LastRunAt     time.Time `json:"last_run_at,omitempty"`
}

// TriggerSourceDiagnostics reports one source's consumer position.
type TriggerSourceDiagnostics struct {
	Source string `json:"source"`
	// Cursor is the log sequence the source has been consumed up to.
	Cursor uint64 `json:"cursor"`
	// Pending reports whether the source holds a record the cursor has not
	// passed. It is read as one record, not as the stream.
	Pending bool `json:"pending"`
}

// TriggerDiagnostics reports one scope's consumer positions and its lease. It
// is a reading: it consumes nothing and takes nothing.
type TriggerDiagnostics struct {
	Scope   corememory.Scope           `json:"scope"`
	Sources []TriggerSourceDiagnostics `json:"sources,omitempty"`
	Lease   *ScopeLease                `json:"lease,omitempty"`
}

// TriggerReport is what one scope's drain did. It is returned per scope so a
// host can tell "nothing to do" from "another runner has it".
type TriggerReport struct {
	Scope corememory.Scope `json:"scope"`
	// Held reports whether this drain took the scope's lease. A scope another
	// holder leased is reported unheld, with the claim that kept it.
	Held   bool   `json:"held"`
	Holder string `json:"holder,omitempty"`
	// Repairing reports a scope whose projection lanes are being rebuilt: a
	// pass would index beside the rebuild, so the drain is left for later.
	Repairing bool `json:"repairing,omitempty"`
	// Records is how many records the poll read across its sources,
	// Conversations how many distinct conversations they named.
	Records       int `json:"records"`
	Conversations int `json:"conversations"`
	// Derived counts the conversation passes that ran, Documents the document
	// passes (a scope has one), Retried the conversations left for the next
	// poll -- one in flight, or one whose pass failed.
	Derived   int `json:"derived"`
	Documents int `json:"documents"`
	Retried   int `json:"retried"`
	// ConversationIDs names the conversations this drain ran a pass over, in
	// the order their first record appeared. A host that maintains what
	// changed -- `maintain.Service.RunConversations` -- reads it instead of
	// listing the scope's conversations to guess which ones it was.
	ConversationIDs []string `json:"conversation_ids,omitempty"`
	// Consumed counts the records this drain advanced cursors past: the leading
	// records of each source whose work was done.
	Consumed int `json:"consumed"`
	// LeaseLost reports a drain that stopped because another holder took the
	// scope while it worked: the passes it ran are durable, and the rest of
	// the batch is the other holder's.
	LeaseLost bool `json:"lease_lost,omitempty"`
	// Cursors is the consumer position of each source after the drain.
	Cursors map[string]uint64 `json:"cursors,omitempty"`
}

// TriggerConfig wires the publication trigger.
type TriggerConfig struct {
	// Journal is the scope-level work journal the write paths record into.
	Journal *TriggerJournal
	// Outbox is the fact view's publication stream. It is optional: a
	// deployment without it still derives turns and documents.
	Outbox *factview.Outbox
	// Processor runs the passes the records ask for.
	Processor *Processor
	// Leases is the scope-level lease the runners share.
	Leases *LeaseStore
	// Cursors is the durable consumer position of this trigger.
	Cursors *TriggerStore
	// Holder names this runner, for diagnostics and for the lease. Two runners
	// have to differ, or each takes the other's claim for its own. Empty mints
	// one.
	Holder string
	// LeaseTTL bounds one scope's claim. Zero takes the default.
	LeaseTTL time.Duration
	// Batch bounds how many records one poll reads from one source. Zero takes
	// the default.
	Batch int
	// Clock stamps the cursor writes and measures the claim. It defaults to
	// time.Now.
	Clock func() time.Time
}

// Trigger turns durable work records into derivation passes.
//
// It is the fast path beside the sweep: the sweep walks every conversation of
// every scope on a fixed period, while the trigger derives the conversations
// and scopes the journal and the fact outbox recorded work for, as soon as the
// record is durable. The two are the same derivation -- a trigger pass is
// ProcessConversation or the document pass -- so a conversation derived by
// either is a conversation the other has nothing left to do for; the
// watermarks and the published generation say so.
//
// The trigger is deliberately not a scheduler: it holds no timer, it is one
// drain a caller can run whenever it likes, and the record it consumes is a
// trigger rather than an instruction. A record it reads twice asks for work
// that is already done, which is what the per-commit idempotence of derivation
// is for.
type Trigger struct {
	journal   *TriggerJournal
	outbox    *factview.Outbox
	processor *Processor
	leases    *LeaseStore
	cursors   *TriggerStore
	sources   []string
	holder    string
	leaseTTL  time.Duration
	batch     int
	clock     func() time.Time

	statsMu sync.Mutex
	stats   TriggerStats
}

// Trigger source names. They are the keys the cursors are stored under and the
// names diagnostics reports, so a reading says which source a cursor belongs to.
const (
	// TriggerSourceJournal is the scope-level work journal the write paths
	// record into.
	TriggerSourceJournal = "journal"
	// TriggerSourcePublications is the fact view's publication outbox, the
	// source that says which conversations gained facts that arrived outside a
	// derivation pass.
	TriggerSourcePublications = "publications"
)

// NewTrigger validates the trigger wiring and mints a holder for a runner that
// did not bring one.
func NewTrigger(config TriggerConfig) (*Trigger, error) {
	if config.Journal == nil {
		return nil, errors.New("memory worker: trigger requires the work journal")
	}
	if config.Processor == nil {
		return nil, errors.New("memory worker: trigger requires a processor")
	}
	if config.Leases == nil {
		return nil, errors.New("memory worker: trigger requires a lease store")
	}
	if config.Cursors == nil {
		return nil, errors.New("memory worker: trigger requires a cursor store")
	}
	holder := config.Holder
	if strings.TrimSpace(holder) == "" {
		generated, err := newTriggerHolder()
		if err != nil {
			return nil, err
		}
		holder = generated
	}
	if err := validateHolder(holder); err != nil {
		return nil, err
	}
	ttl := config.LeaseTTL
	if ttl == 0 {
		ttl = defaultLeaseTTL
	}
	if ttl < 0 {
		return nil, errors.New("memory worker: trigger lease ttl must not be negative")
	}
	batch := config.Batch
	if batch == 0 {
		batch = defaultTriggerBatch
	}
	if batch < 0 {
		return nil, errors.New("memory worker: trigger batch must not be negative")
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	sources := []string{TriggerSourceJournal}
	if config.Outbox != nil {
		sources = append(sources, TriggerSourcePublications)
	}
	return &Trigger{
		journal: config.Journal, outbox: config.Outbox, processor: config.Processor,
		leases: config.Leases, cursors: config.Cursors, sources: sources,
		holder: holder, leaseTTL: ttl, batch: batch, clock: clock,
	}, nil
}

// Holder names the runner behind this trigger.
func (trigger *Trigger) Holder() string {
	if trigger == nil {
		return ""
	}
	return trigger.holder
}

// Stats returns the cumulative trigger counters.
func (trigger *Trigger) Stats() TriggerStats {
	if trigger == nil {
		return TriggerStats{}
	}
	trigger.statsMu.Lock()
	defer trigger.statsMu.Unlock()
	return trigger.stats
}

// RunScope drains one scope's work records once.
//
// The drain takes the scope's lease, reads one batch per source after that
// source's cursor, runs the document pass the batch asks for and then one pass
// per conversation, and advances each cursor past the leading records it
// cleared. A conversation another pass is already deriving is left for the
// next poll, and so is everything after it: one cursor cannot skip a record, so
// a drain consumes the longest prefix of a batch whose work is done.
//
// A pass that fails fails the drain, but does not stop it: the conversations
// after the failure are derived as well, and the cursors stop at the first
// record the drain did not clear, so the failed work is retried.
func (trigger *Trigger) RunScope(
	ctx context.Context,
	scope corememory.Scope,
) (TriggerReport, error) {
	report := TriggerReport{Scope: scope}
	if trigger == nil {
		return report, errors.New("memory worker: trigger is required")
	}
	if ctx == nil {
		return report, errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return report, err
	}
	lease, held, err := trigger.leases.Acquire(ctx, scope, trigger.holder, trigger.leaseTTL)
	if err != nil {
		return report, err
	}
	report.Holder = lease.Holder
	report.Held = held
	trigger.bump(func(stats *TriggerStats) {
		stats.Runs++
		stats.LastRunAt = trigger.clock().UTC()
		if held {
			stats.ScopesLeased++
			return
		}
		stats.ScopesBusy++
	})
	if !held {
		return report, nil
	}
	// The scope is given back however the drain ends. Releasing under a canceled
	// context is deliberate: a runner that is shutting down has no use for the
	// scope, and the release is what lets the next one start instead of waiting
	// for the claim to expire.
	defer func() {
		if _, err := trigger.leases.Release(context.WithoutCancel(ctx), lease); err != nil {
			trigger.RecordError(err)
		}
	}()
	if trigger.processor.underRepair(scope) {
		report.Repairing = true
		trigger.bump(func(stats *TriggerStats) { stats.ScopesRepairing++ })
		return report, nil
	}
	batches := make([]triggerBatch, 0, len(trigger.sources))
	for _, source := range trigger.sources {
		batch, err := trigger.readSource(ctx, scope, source, trigger.batch)
		if err != nil {
			return report, err
		}
		report.Records += len(batch.entries)
		batches = append(batches, batch)
	}
	if report.Records == 0 {
		return report, nil
	}
	outcome := trigger.drain(ctx, scope, batches, &lease)
	report.Conversations = outcome.conversations
	report.Derived = outcome.derived
	report.Documents = outcome.documents
	report.Retried = outcome.retried
	report.ConversationIDs = outcome.conversationIDs
	report.LeaseLost = outcome.leaseLost
	report.Cursors = make(map[string]uint64, len(batches))
	for index, batch := range batches {
		consumed := outcome.consumed[index]
		cursor := batch.cursor
		if consumed > 0 {
			cursor = batch.entries[consumed-1].Seq
			if err := trigger.cursors.SaveCursor(ctx, TriggerCursor{
				Scope: scope, Source: batch.source, Seq: cursor, UpdatedAt: trigger.clock().UTC(),
			}); err != nil {
				return report, err
			}
		}
		report.Cursors[batch.source] = cursor
		report.Consumed += consumed
	}
	trigger.bump(func(stats *TriggerStats) {
		stats.RecordsRead += int64(report.Records)
		stats.Consumed += int64(report.Consumed)
		stats.Conversations += int64(report.Conversations)
		stats.Derived += int64(report.Derived)
		stats.Documents += int64(report.Documents)
		stats.Retried += int64(report.Retried)
		if report.LeaseLost {
			stats.LeasesLost++
		}
	})
	return report, outcome.err
}

// triggerBatch is one source's read: the records after its cursor, and the
// cursor they were read from.
type triggerBatch struct {
	source  string
	entries []TriggerEntry
	cursor  uint64
}

// readSource reads up to limit records of one source. The publication source is
// mapped onto work records: a publication names the conversation whose pass
// owes it, and its identity is the publication id.
func (trigger *Trigger) readSource(
	ctx context.Context,
	scope corememory.Scope,
	source string,
	limit int,
) (triggerBatch, error) {
	stored, _, err := trigger.cursors.LoadCursor(ctx, scope, source)
	if err != nil {
		return triggerBatch{}, err
	}
	batch := triggerBatch{source: source, cursor: stored.Seq}
	switch source {
	case TriggerSourceJournal:
		entries, _, err := trigger.journal.Read(ctx, scope, stored.Seq, limit)
		if err != nil {
			return triggerBatch{}, err
		}
		batch.entries = entries
	case TriggerSourcePublications:
		entries, _, err := trigger.outbox.ReadEntries(ctx, scope, stored.Seq, limit)
		if err != nil {
			return triggerBatch{}, err
		}
		batch.entries = make([]TriggerEntry, 0, len(entries))
		for _, entry := range entries {
			batch.entries = append(batch.entries, TriggerEntry{
				Seq: entry.Seq,
				Record: TriggerRecord{
					SchemaVersion: triggerSchema, Kind: TriggerKindPublication,
					RuntimeID: scope.RuntimeID, UserID: scope.UserID, AgentID: scope.AgentID,
					ConversationID: entry.Record.ConversationID,
					Ref:            "publication:" + entry.Record.PublicationID,
				},
			})
		}
	default:
		return triggerBatch{}, fmt.Errorf("memory worker: trigger source %q is not supported", source)
	}
	return batch, nil
}

// drainSummary is what one drain did, before it is reported.
type drainSummary struct {
	conversations int
	// conversationIDs is the derived subset of those conversations: the ones a
	// pass actually ran for, in the order the drain reached them.
	conversationIDs []string
	derived         int
	documents       int
	retried         int
	consumed        []int
	leaseLost       bool
	err             error
}

// drain runs the work the batches name: the document pass once, then one pass
// per conversation, in the order the records were read. The lease is renewed as
// the drain works, so a long drain of many conversations does not outlive its
// own claim; a renewal that fails stops the drain where it is.
func (trigger *Trigger) drain(
	ctx context.Context,
	scope corememory.Scope,
	batches []triggerBatch,
	lease *ScopeLease,
) drainSummary {
	ordered := make([]string, 0)
	seen := make(map[string]struct{})
	documents := 0
	for _, batch := range batches {
		for _, entry := range batch.entries {
			switch entry.Record.Kind {
			case TriggerKindDocument:
				documents++
			case TriggerKindTurn, TriggerKindPublication:
				conversationID := strings.TrimSpace(entry.Record.ConversationID)
				if _, duplicate := seen[conversationID]; duplicate {
					continue
				}
				seen[conversationID] = struct{}{}
				ordered = append(ordered, conversationID)
			}
		}
	}
	summary := drainSummary{conversations: len(ordered)}
	done := make(map[string]struct{}, len(ordered))
	documentsDone := documents == 0
	var failures []error
	stopped := false
	if !documentsDone {
		if !trigger.renewForDrain(ctx, lease, &summary, &failures) {
			stopped = true
		} else if err := trigger.processor.ProcessDocuments(ctx, scope); err != nil {
			failures = append(failures, fmt.Errorf("memory worker: trigger documents: %w", err))
		} else {
			documentsDone = true
			summary.documents = 1
		}
	}
	for _, conversationID := range ordered {
		if stopped {
			break
		}
		if trigger.processor.deriving(scope, conversationID) {
			// A pass over this conversation is running in this processor. The
			// work is being done -- by the pass that started it, which reports
			// its own outcome -- so the drain leaves the record behind.
			summary.retried++
			continue
		}
		if !trigger.renewForDrain(ctx, lease, &summary, &failures) {
			break
		}
		if _, err := trigger.processor.ProcessConversation(ctx, scope, conversationID); err != nil {
			failures = append(failures, fmt.Errorf(
				"memory worker: trigger conversation %q: %w", conversationID, err))
			summary.retried++
			continue
		}
		done[conversationID] = struct{}{}
		summary.conversationIDs = append(summary.conversationIDs, conversationID)
		summary.derived++
	}
	summary.err = errors.Join(failures...)
	summary.consumed = consumePrefixes(batches, documentsDone, done)
	return summary
}

// renewForDrain extends the claim if it is due and folds the outcome into the
// drain. It returns false when the drain has to stop: a claim another holder
// took, or a renewal that failed.
func (trigger *Trigger) renewForDrain(
	ctx context.Context,
	lease *ScopeLease,
	summary *drainSummary,
	failures *[]error,
) bool {
	renewed, held, err := trigger.renewIfDue(ctx, *lease)
	if err != nil {
		*failures = append(*failures, err)
		return false
	}
	if !held {
		summary.leaseLost = true
		return false
	}
	*lease = renewed
	return true
}

// consumePrefixes reports, per batch, how many leading records the drain
// cleared: a document record is cleared when the document pass ran, a
// conversation record when a pass over its conversation ran.
func consumePrefixes(batches []triggerBatch, documentsDone bool, done map[string]struct{}) []int {
	consumed := make([]int, len(batches))
	for index, batch := range batches {
		for _, entry := range batch.entries {
			cleared := documentsDone && entry.Record.Kind == TriggerKindDocument
			if entry.Record.Kind != TriggerKindDocument {
				_, cleared = done[strings.TrimSpace(entry.Record.ConversationID)]
			}
			if !cleared {
				break
			}
			consumed[index]++
		}
	}
	return consumed
}

// renewIfDue extends the claim once at least half of it is spent: the renewal
// is what keeps a drain of many conversations one holder's, and renewing on
// every conversation would write the claim once per conversation.
func (trigger *Trigger) renewIfDue(
	ctx context.Context,
	lease ScopeLease,
) (ScopeLease, bool, error) {
	now := trigger.clock().UTC()
	if now.Before(lease.ExpiresAt.Add(-trigger.leaseTTL / 2)) {
		return lease, true, nil
	}
	renewed, held, err := trigger.leases.Renew(ctx, lease, trigger.leaseTTL)
	if err != nil {
		return lease, false, err
	}
	if !held {
		return lease, false, nil
	}
	return renewed, true, nil
}

// Diagnostics reports one scope's consumer positions and lease with bounded
// reads: one cursor per source, one record per source, one claim.
func (trigger *Trigger) Diagnostics(
	ctx context.Context,
	scope corememory.Scope,
) (TriggerDiagnostics, error) {
	if trigger == nil {
		return TriggerDiagnostics{}, errors.New("memory worker: trigger is required")
	}
	if ctx == nil {
		return TriggerDiagnostics{}, errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return TriggerDiagnostics{}, err
	}
	result := TriggerDiagnostics{Scope: scope}
	for _, source := range trigger.sources {
		batch, err := trigger.readSource(ctx, scope, source, 1)
		if err != nil {
			return TriggerDiagnostics{}, err
		}
		result.Sources = append(result.Sources, TriggerSourceDiagnostics{
			Source: source, Cursor: batch.cursor, Pending: len(batch.entries) > 0,
		})
	}
	lease, found, err := trigger.leases.Current(ctx, scope)
	if err != nil {
		return TriggerDiagnostics{}, err
	}
	if found {
		result.Lease = &lease
	}
	return result, nil
}

// RecordError stores the most recent trigger failure for diagnostics.
func (trigger *Trigger) RecordError(err error) {
	if trigger == nil || err == nil {
		return
	}
	trigger.statsMu.Lock()
	trigger.stats.LastError = err.Error()
	trigger.statsMu.Unlock()
}

func (trigger *Trigger) bump(update func(*TriggerStats)) {
	trigger.statsMu.Lock()
	update(&trigger.stats)
	trigger.statsMu.Unlock()
}

// newTriggerHolder mints a holder name unique to one runner.
func newTriggerHolder() (string, error) {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("memory worker: trigger holder: %w", err)
	}
	return "trigger-" + hex.EncodeToString(buffer), nil
}
