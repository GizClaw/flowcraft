package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// countingStore is a KV substrate that counts what a poll pulls out of it. The
// numbers are a reading, not an assertion: a trigger poll is supposed to be
// cheap when there is nothing to do, and a test that reports what one costs is
// what says whether it is.
type countingStore struct {
	storage.Store
	storage.CASStore
	storage.PutIfAbsentStore

	gets      int
	readBytes int
	// lists counts the prefix scans and what they carried. A scan reads the
	// values of every key under its prefix, so this is where a pass that lists
	// a whole area shows up.
	lists       int
	listEntries int
	listBytes   int
	puts        int
	writeBytes  int
	// cas counts the compare-and-swap writes, which is how the claim is taken
	// and given back: a store that only counted Put would report an idle poll
	// as writing nothing at all.
	cas int
}

func (store *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	value, err := store.Store.Get(ctx, key)
	if err == nil {
		store.gets++
		store.readBytes += len(key) + len(value)
	}
	return value, err
}

func (store *countingStore) Put(ctx context.Context, key string, data []byte) error {
	store.puts++
	store.writeBytes += len(key) + len(data)
	return store.Store.Put(ctx, key, data)
}

func (store *countingStore) List(ctx context.Context, prefix string) ([]storage.Entry, error) {
	entries, err := store.Store.List(ctx, prefix)
	if err == nil {
		store.lists++
		store.listEntries += len(entries)
		for _, entry := range entries {
			store.listBytes += len(entry.Key) + len(entry.Value)
		}
	}
	return entries, err
}

func (store *countingStore) CompareAndSwap(ctx context.Context, key string, old, new []byte) (bool, error) {
	store.cas++
	return store.CASStore.CompareAndSwap(ctx, key, old, new)
}

func (store *countingStore) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	store.puts++
	store.writeBytes += len(key) + len(data)
	return store.PutIfAbsentStore.PutIfAbsent(ctx, key, data)
}

func (store *countingStore) reset() {
	store.gets, store.readBytes, store.puts, store.writeBytes, store.cas = 0, 0, 0, 0, 0
	store.lists, store.listEntries, store.listBytes = 0, 0, 0
}

// countingLog is the same for the log substrate: every record a pass reads
// through it, which is where a derivation's cost actually is.
type countingLog struct {
	storage.Log
	storage.CommitLog

	reads     int
	events    int
	readBytes int
}

func (log *countingLog) Read(ctx context.Context, stream string, after uint64, limit int) ([]storage.Event, error) {
	events, err := log.Log.Read(ctx, stream, after, limit)
	if err == nil {
		log.reads++
		log.events += len(events)
		for _, event := range events {
			log.readBytes += len(event.Payload)
		}
	}
	return events, err
}

func (log *countingLog) reset() {
	log.reads, log.events, log.readBytes = 0, 0, 0
}

// scaleTrigger is one trigger over the real substrates, with the real message
// store, so a reading covers what a deployment pays and not what a fake source
// pretends to cost.
type scaleTrigger struct {
	trigger   *Trigger
	journal   *TriggerJournal
	messages  *msgsource.MessageStore
	facts     *factview.FactStore
	processor *Processor
	kv        *countingStore
	log       *countingLog
	scope     corememory.Scope
}

func newScaleTrigger(t *testing.T) *scaleTrigger {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	rawLog, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	rawKV, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	logStore := &countingLog{Log: rawLog, CommitLog: rawLog}
	kvStore := &countingStore{Store: rawKV, CASStore: rawKV, PutIfAbsentStore: rawKV}
	clock := func() time.Time { return testLeaseNow() }
	messages, err := msgsource.NewMessageStore(logStore, msgsource.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := factview.NewOutbox(logStore)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := factview.NewFactStore(logStore, kvStore,
		factview.WithPublicationSink(outbox), factview.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := NewTriggerJournal(logStore)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := newMemoryCheckpoints()
	processor, err := NewProcessor(Config{
		Messages: messages, Facts: facts, Deriver: hashDeriver{}, Checkpoints: checkpoints,
		Projection: "test", PolicyDigest: "digest-test",
		Indexers: []ProjectionIndexer{{Name: "test", Indexer: &countingIndexer{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	leases, err := NewLeaseStore(kvStore, WithLeaseClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	cursors, err := NewTriggerStore(kvStore, WithTriggerClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := NewTrigger(TriggerConfig{
		Journal: journal, Outbox: outbox, Processor: processor,
		Leases: leases, Cursors: cursors, Holder: "runner-a", Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &scaleTrigger{
		trigger: trigger, journal: journal, messages: messages, facts: facts,
		processor: processor, kv: kvStore, log: logStore,
		scope: corememory.Scope{RuntimeID: "memories", UserID: "u1"},
	}
}

// commitTurn writes one turn through the message store and records the work it
// owes, which is what a write path does.
func (fixture *scaleTrigger) commitTurn(t *testing.T, conversationID, key string) {
	t.Helper()
	ctx := context.Background()
	commit, err := fixture.messages.Commit(ctx, msgsource.AppendRequest{
		Scope: fixture.scope, ConversationID: conversationID, IdempotencyKey: key,
		Messages: []coremessage.Message{
			coremessage.NewTextMessage(coremessage.RoleUser, "hello from "+conversationID),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.RecordTurn(ctx, fixture.scope, conversationID, commit.ID); err != nil {
		t.Fatal(err)
	}
}

func (fixture *scaleTrigger) settle(t *testing.T) {
	t.Helper()
	for poll := 0; poll < 8; poll++ {
		report, err := fixture.trigger.RunScope(context.Background(), fixture.scope)
		if err != nil {
			t.Fatal(err)
		}
		if report.Records == 0 {
			return
		}
	}
	t.Fatal("the polls never settled")
}

func (fixture *scaleTrigger) reset() {
	fixture.kv.reset()
	fixture.log.reset()
}

// TestTriggerIdlePollReadsNoRecords is the reading a deployment pays for
// having the fast path on: a poll of a scope with nothing to do reads the
// cursors and the claim, and follows no record anywhere.
func TestTriggerIdlePollReadsNoRecords(t *testing.T) {
	fixture := newScaleTrigger(t)
	fixture.settle(t)
	fixture.reset()
	start := time.Now()
	report, err := fixture.trigger.RunScope(context.Background(), fixture.scope)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 0 || report.Derived != 0 {
		t.Fatalf("an idle poll did work: %#v", report)
	}
	t.Logf("idle poll: %v; kv %d reads / %d bytes, %d writes / %d bytes, %d cas; log %d reads / %d records / %d bytes",
		elapsed, fixture.kv.gets, fixture.kv.readBytes, fixture.kv.puts, fixture.kv.writeBytes, fixture.kv.cas,
		fixture.log.reads, fixture.log.events, fixture.log.readBytes)
	if fixture.log.events != 0 {
		t.Fatalf("an idle poll read %d log records", fixture.log.events)
	}
	if fixture.kv.gets > 8 {
		t.Fatalf("an idle poll made %d kv reads", fixture.kv.gets)
	}
}

// TestTriggerPollCostForOneTurn is the fast path's own reading, and the byte
// count the plan asks for: what one poll costs to make one committed turn
// retrievable -- the work record, the commit listing it leads to, and the
// derivation's writes.
func TestTriggerPollCostForOneTurn(t *testing.T) {
	fixture := newScaleTrigger(t)
	fixture.commitTurn(t, "conv-a", "turn-1")
	fixture.reset()
	start := time.Now()
	report, err := fixture.trigger.RunScope(context.Background(), fixture.scope)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 1 || report.Derived != 1 || report.Consumed != 1 {
		t.Fatalf("drain = %#v", report)
	}
	t.Logf("poll of one turn: %v; kv %d reads / %d bytes, %d writes / %d bytes, %d cas; log %d reads / %d records / %d bytes",
		elapsed, fixture.kv.gets, fixture.kv.readBytes, fixture.kv.puts, fixture.kv.writeBytes, fixture.kv.cas,
		fixture.log.reads, fixture.log.events, fixture.log.readBytes)
	facts, err := fixture.facts.List(context.Background(), fixture.scope, "conv-a", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts after one poll = %#v", facts)
	}
	if fixture.log.events == 0 {
		t.Fatal("the pass read no records: the commit listing is not covered by the reading")
	}
}

// TestTriggerPollDerivesEveryRecordOfABatch is the same reading at the size of
// a batch: one poll derives every conversation its records name, and the cost
// is per conversation, which is what the sweep could not do for a scope.
func TestTriggerPollDerivesEveryRecordOfABatch(t *testing.T) {
	const conversations = 50
	fixture := newScaleTrigger(t)
	for index := 0; index < conversations; index++ {
		fixture.commitTurn(t, fmt.Sprintf("conv-%d", index), fmt.Sprintf("turn-%d", index))
	}
	fixture.reset()
	start := time.Now()
	report, err := fixture.trigger.RunScope(context.Background(), fixture.scope)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != conversations || report.Derived != conversations || report.Consumed != conversations {
		t.Fatalf("drain of %d turns = %#v", conversations, report)
	}
	t.Logf("poll of %d turns: %v (%v per conversation); kv %d reads / %d bytes, %d writes / %d bytes, %d cas; log %d reads / %d records / %d bytes",
		conversations, elapsed, elapsed/time.Duration(conversations),
		fixture.kv.gets, fixture.kv.readBytes, fixture.kv.puts, fixture.kv.writeBytes, fixture.kv.cas,
		fixture.log.reads, fixture.log.events, fixture.log.readBytes)
}
