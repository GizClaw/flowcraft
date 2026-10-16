package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// hookedMessages delegates to another message source and runs a hook before
// every commit listing. A test uses it to move the world -- take the scope's
// lease away, in practice -- while a pass is running.
type hookedMessages struct {
	source MessageReader
	hook   func()
}

func (source *hookedMessages) ListConversations(
	ctx context.Context,
	scope corememory.Scope,
) ([]string, error) {
	return source.source.ListConversations(ctx, scope)
}

func (source *hookedMessages) ListCommits(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	options msgsource.ListCommitOptions,
) ([]msgsource.Commit, error) {
	if source.hook != nil {
		source.hook()
	}
	return source.source.ListCommits(ctx, scope, conversationID, options)
}

func (source *hookedMessages) List(
	ctx context.Context,
	scope corememory.Scope,
	conversationID string,
	options msgsource.ListOptions,
) ([]msgsource.Record, error) {
	return source.source.List(ctx, scope, conversationID, options)
}

// triggerFixture is one trigger over a workspace: the journal a write path
// records into, the fact outbox, the cursor and lease stores, the processor
// that derives, and a clock a test moves by hand.
type triggerFixture struct {
	trigger     *Trigger
	journal     *TriggerJournal
	outbox      *factview.Outbox
	facts       *factview.FactStore
	leases      *LeaseStore
	cursors     *TriggerStore
	messages    *hookedMessages
	processor   *Processor
	checkpoints *memoryCheckpoints
	indexer     *countingIndexer
	kv          storage.Store
	scope       corememory.Scope
	clock       *leaseClock
}

type triggerFixtureConfig struct {
	conversations []string
	holder        string
	ttl           time.Duration
	batch         int
	delay         time.Duration
	failOn        string
	// withoutOutbox builds the assembly a deployment that turned the fact
	// view's outbox off has: no publications to consume, and the journal is
	// the only source the trigger reads.
	withoutOutbox bool
}

func newTriggerFixture(t *testing.T, options ...func(*triggerFixtureConfig)) *triggerFixture {
	t.Helper()
	config := triggerFixtureConfig{holder: "runner-a", ttl: time.Minute}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	scope := corememory.Scope{RuntimeID: "memories", UserID: "u1"}
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := factview.NewOutbox(logStore)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := NewTriggerJournal(logStore)
	if err != nil {
		t.Fatal(err)
	}
	factOptions := []factview.Option{}
	triggerOutbox := outbox
	if config.withoutOutbox {
		triggerOutbox = nil
	} else {
		factOptions = append(factOptions, factview.WithPublicationSink(outbox))
	}
	facts, err := factview.NewFactStore(logStore, kvStore, factOptions...)
	if err != nil {
		t.Fatal(err)
	}
	inner := &fakeMessages{
		commits: triggerCommits(scope, config.conversations...),
		delay:   config.delay, failOn: config.failOn,
	}
	messages := &hookedMessages{source: inner}
	checkpoints := newMemoryCheckpoints()
	indexer := &countingIndexer{}
	processor, err := NewProcessor(Config{
		Messages: messages, Facts: facts, Deriver: hashDeriver{}, Checkpoints: checkpoints,
		Projection: "test", PolicyDigest: "digest-test",
		Indexers: []ProjectionIndexer{{Name: "test", Indexer: indexer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	clock := newLeaseClock(testLeaseNow())
	leases, err := NewLeaseStore(kvStore, WithLeaseClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	cursors, err := NewTriggerStore(kvStore, WithTriggerClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := NewTrigger(TriggerConfig{
		Journal: journal, Outbox: triggerOutbox, Processor: processor,
		Leases: leases, Cursors: cursors, Holder: config.holder,
		LeaseTTL: config.ttl, Batch: config.batch, Clock: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &triggerFixture{
		trigger: trigger, journal: journal, outbox: outbox, facts: facts,
		leases: leases, cursors: cursors, messages: messages, processor: processor,
		checkpoints: checkpoints, indexer: indexer, kv: kvStore,
		scope: scope, clock: clock,
	}
}

// triggerCommits builds one commit per conversation, scoped to the fixture's
// own scope: a pass reads the commits of the scope it was asked about.
func triggerCommits(scope corememory.Scope, conversations ...string) map[string][]msgsource.Commit {
	commits := make(map[string][]msgsource.Commit, len(conversations))
	for _, conversationID := range conversations {
		commits[conversationID] = []msgsource.Commit{{
			ID: triggerCommitID(conversationID), Scope: scope, Version: 1,
			ConversationID: conversationID,
			Records: []msgsource.Record{{
				ID: "msg-" + conversationID, ConversationID: conversationID, Seq: 1,
				Message: coremessage.NewTextMessage(coremessage.RoleUser, "hello from "+conversationID),
			}},
		}}
	}
	return commits
}

// triggerCommitID names the one commit a fixture conversation holds, so the
// work record a test writes refers to the commit that is actually there.
func triggerCommitID(conversationID string) string {
	return "commit-" + conversationID
}

func (fixture *triggerFixture) recordTurns(t *testing.T, conversations ...string) {
	t.Helper()
	for _, conversationID := range conversations {
		if err := fixture.journal.RecordTurn(
			context.Background(), fixture.scope, conversationID, triggerCommitID(conversationID),
		); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTriggerDerivesTheConversationATurnRecordNames pins the fast path end to
// end: a work record for a conversation makes the trigger derive it -- facts
// published into the generation, deltas in the lanes, the generation published
// for readers -- and advance the cursor past the record.
func TestTriggerDerivesTheConversationATurnRecordNames(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a"}
	})
	fixture.recordTurns(t, "conv-a")

	report, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Held || report.Derived != 1 || report.Consumed != 1 || report.Records != 1 {
		t.Fatalf("report = %#v", report)
	}
	if len(report.ConversationIDs) != 1 || report.ConversationIDs[0] != "conv-a" {
		t.Fatalf("the drain named the conversations it derived as %#v", report.ConversationIDs)
	}
	if report.Cursors[TriggerSourceJournal] == 0 {
		t.Fatalf("journal cursor did not advance: %#v", report.Cursors)
	}
	values, err := fixture.facts.List(ctx, fixture.scope, "conv-a", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("facts after the trigger = %#v", values)
	}
	if fixture.indexer.count() == 0 {
		t.Fatal("the pass indexed nothing")
	}
	active, found, err := fixture.facts.ActiveGeneration(ctx, fixture.scope, "conv-a")
	if err != nil || !found {
		t.Fatalf("active generation = %q, %v, %v", active, found, err)
	}
	if active != "digest-test" {
		t.Fatalf("active generation = %q, want the running policy", active)
	}
}

// TestTriggerConsumesEachRecordOnce pins that a drained journal asks for
// nothing: the second poll reads no record and derives nothing, which is what
// makes the trigger safe to run on a short period.
func TestTriggerConsumesEachRecordOnce(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a", "conv-b"}
	})
	fixture.recordTurns(t, "conv-a", "conv-b")

	first, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if first.Derived != 2 || first.Consumed != 2 {
		t.Fatalf("first drain = %#v", first)
	}
	second, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if second.Cursors[TriggerSourceJournal] != first.Cursors[TriggerSourceJournal] {
		t.Fatalf("the journal moved on an empty read: %#v then %#v", first.Cursors, second.Cursors)
	}
	report, err := fixture.trigger.Diagnostics(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if journal := triggerSource(t, report, TriggerSourceJournal); journal.Pending {
		t.Fatalf("the journal still holds a record the first poll consumed: %#v", journal)
	}
	stats := fixture.trigger.Stats()
	// Two polls ran; the passes the first one caused the second one to repeat
	// are counted as derived too, and none of the work was left retried.
	if stats.Runs != 2 || stats.Retried != 0 {
		t.Fatalf("stats after two polls = %#v", stats)
	}
}

// TestTriggerSettlesOnceItsOwnPublicationsAreConsumed pins the bound on the
// fact view's feedback, which is the one thing a pass can leave behind for
// itself: the view publishes every fact it stores, a derived one included, so
// the poll after a pass reads that publication and runs one more pass over the
// conversation. That pass derives nothing -- the commits are behind the
// watermark and the generation is already published -- so it publishes
// nothing, and the polls settle instead of feeding themselves.
func TestTriggerSettlesOnceItsOwnPublicationsAreConsumed(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a"}
	})
	fixture.recordTurns(t, "conv-a")
	polls := 0
	for ; polls < 5; polls++ {
		report, err := fixture.trigger.RunScope(ctx, fixture.scope)
		if err != nil {
			t.Fatal(err)
		}
		if report.Records == 0 {
			break
		}
	}
	if polls != 2 {
		t.Fatalf("the polls settled after %d polls with work, want the deriving one and the one it caused", polls)
	}
	values, err := fixture.facts.List(ctx, fixture.scope, "conv-a", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("the second pass derived the fact again: %#v", values)
	}
}

// TestTriggerLeavesAConversationInFlightForTheNextPoll pins the refusal that
// keeps two passes off one conversation: the record is left where it is -- and
// so is everything after it, since one cursor cannot skip a record -- and the
// next poll derives it once the pass that held it is done.
func TestTriggerLeavesAConversationInFlightForTheNextPoll(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a"}
	})
	fixture.recordTurns(t, "conv-a")
	finished, err := fixture.processor.beginDerivation(fixture.scope, "conv-a")
	if err != nil {
		t.Fatal(err)
	}
	report, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if report.Derived != 0 || report.Retried != 1 || report.Consumed != 0 {
		t.Fatalf("drain beside an in-flight pass = %#v", report)
	}
	if report.Cursors[TriggerSourceJournal] != 0 {
		t.Fatalf("cursor moved past a record it did not clear: %#v", report.Cursors)
	}
	finished()
	report, err = fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if report.Derived != 1 || report.Consumed != 1 {
		t.Fatalf("drain after the pass finished = %#v", report)
	}
}

// TestTriggerKeepsAFailedConversationForTheNextPoll pins the retry: a pass that
// fails does not stop the drain -- the conversations behind it are derived --
// but the cursor stops at the failed record, so the next poll asks again.
func TestTriggerKeepsAFailedConversationForTheNextPoll(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a", "conv-b", "conv-c"}
		config.failOn = "conv-b"
	})
	fixture.recordTurns(t, "conv-a", "conv-b", "conv-c")

	report, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err == nil {
		t.Fatal("a drain over a failing conversation reported no error")
	}
	if report.Derived != 2 || report.Retried != 1 {
		t.Fatalf("drain = %#v", report)
	}
	if report.Consumed != 1 {
		t.Fatalf("cursor passed a record whose pass failed: %#v", report)
	}
	if !strings.Contains(err.Error(), "conv-b") {
		t.Fatalf("error = %v, want the failed conversation named", err)
	}
	// The failure clears. The next poll reads the record that failed and the
	// records behind it: the conversations already derived are asked again,
	// which is a pass over a conversation that has nothing left to derive.
	fixture.messages.source.(*fakeMessages).failOn = ""
	report, err = fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if report.Retried != 0 {
		t.Fatalf("drain after the failure cleared = %#v", report)
	}
	if report.Cursors[TriggerSourceJournal] == 0 {
		t.Fatalf("the journal did not catch up: %#v", report.Cursors)
	}
	report2, err := fixture.trigger.Diagnostics(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if journal := triggerSource(t, report2, TriggerSourceJournal); journal.Pending {
		t.Fatalf("the journal still holds the record that failed: %#v", journal)
	}
}

// TestTriggerRefusesAScopeAnotherHolderLeases pins the exclusion: a scope
// another runner holds is left alone, reported with the holder that kept it,
// and nothing about the scope moves.
func TestTriggerRefusesAScopeAnotherHolderLeases(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a"}
	})
	fixture.recordTurns(t, "conv-a")
	if _, held, err := fixture.leases.Acquire(ctx, fixture.scope, "runner-b", time.Minute); err != nil || !held {
		t.Fatalf("the other runner took the scope: %v, %v", held, err)
	}
	report, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if report.Held || report.Holder != "runner-b" || report.Records != 0 {
		t.Fatalf("drain of a leased scope = %#v", report)
	}
	if stats := fixture.trigger.Stats(); stats.ScopesBusy != 1 || stats.Derived != 0 {
		t.Fatalf("stats = %#v", stats)
	}
	if held, err := fixture.leases.Release(ctx, ScopeLease{Scope: fixture.scope, Holder: "runner-b"}); err != nil || !held {
		t.Fatalf("the other runner released its claim: %v, %v", held, err)
	}
}

// TestTriggerStopsWhenItsLeaseIsTaken pins that a drain renews what it holds
// and stops when it no longer holds it: a runner that lost the scope mid-drain
// keeps what it derived and leaves the rest of the batch to the holder.
func TestTriggerStopsWhenItsLeaseIsTaken(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a", "conv-b", "conv-c"}
		config.ttl = time.Minute
	})
	fixture.recordTurns(t, "conv-a", "conv-b", "conv-c")
	stolen := false
	fixture.messages.hook = func() {
		if stolen {
			return
		}
		stolen = true
		// The clock moves past the claim, another runner takes the scope, and
		// the drain's next renewal is the one that finds out.
		fixture.clock.Advance(2 * time.Minute)
		steal, err := NewLeaseStore(fixture.kv, WithLeaseClock(fixture.clock.Now))
		if err != nil {
			t.Fatal(err)
		}
		if _, held, err := steal.Acquire(ctx, fixture.scope, "runner-b", time.Minute); err != nil || !held {
			t.Fatalf("the other runner took the scope: %v, %v", held, err)
		}
	}
	report, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if !report.LeaseLost {
		t.Fatalf("drain over a stolen scope = %#v", report)
	}
	if report.Derived != 1 || report.Consumed != 1 {
		t.Fatalf("drain kept %d passes and consumed %d records, want the first one", report.Derived, report.Consumed)
	}
	if stats := fixture.trigger.Stats(); stats.LeasesLost != 1 {
		t.Fatalf("stats = %#v", stats)
	}
	current, found, err := fixture.leases.Current(ctx, fixture.scope)
	if err != nil || !found {
		t.Fatalf("claim after the steal = %#v, %v, %v", current, found, err)
	}
	if current.Holder != "runner-b" {
		t.Fatalf("the drain released a claim that was not its own: %#v", current)
	}
}

// TestTriggerReadsBoundedBatches pins the batch bound: one poll reads one
// batch per source, and the rest is the next poll's work. The fixture has no
// outbox, so one batch is one record and the arithmetic is the bound itself.
func TestTriggerReadsBoundedBatches(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a", "conv-b"}
		config.batch = 1
		config.withoutOutbox = true
	})
	fixture.recordTurns(t, "conv-a", "conv-b")

	first, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if first.Records != 1 || first.Consumed != 1 || first.Derived != 1 {
		t.Fatalf("first drain = %#v", first)
	}
	second, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if second.Records != 1 || second.Consumed != 1 || second.Derived != 1 {
		t.Fatalf("second drain = %#v", second)
	}
	third, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if third.Records != 0 {
		t.Fatalf("third drain = %#v", third)
	}
	report, err := fixture.trigger.Diagnostics(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sources) != 1 || report.Sources[0].Source != TriggerSourceJournal {
		t.Fatalf("a trigger without an outbox reads more than the journal: %#v", report.Sources)
	}
}

// TestTriggerRunsTheDocumentPassOncePerDrain pins the scope-level half: a
// document record asks for the document pass, one pass clears every document
// record of the drain, and it runs before the conversations.
func TestTriggerRunsTheDocumentPassOncePerDrain(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a"}
	})
	if err := fixture.journal.RecordDocument(ctx, fixture.scope, "document:d1/doc-1@1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.RecordDocument(ctx, fixture.scope, "document:d1/doc-2@1"); err != nil {
		t.Fatal(err)
	}
	fixture.recordTurns(t, "conv-a")

	report, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if report.Documents != 1 || report.Derived != 1 || report.Consumed != 3 {
		t.Fatalf("drain = %#v", report)
	}
	if report.Cursors[TriggerSourceJournal] == 0 {
		t.Fatalf("journal cursor did not advance: %#v", report.Cursors)
	}
}

// TestTriggerPublicationSourceDerivesWhatTheViewPublished pins the second
// source: a fact written through the view is a publication, and the trigger
// derives the conversation it names so the fact is indexed.
func TestTriggerPublicationSourceDerivesWhatTheViewPublished(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a"}
	})
	if _, err := fixture.facts.Add(ctx, factview.AddRequest{
		ID: "fact-imported", Generation: "digest-test", Scope: fixture.scope, ConversationID: "conv-a",
		Content:    coremessage.NewTextContent("imported from somewhere"),
		Provenance: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message-imported", Revision: "1"}},
	}); err != nil {
		t.Fatal(err)
	}
	report, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 1 || report.Derived != 1 || report.Consumed != 1 {
		t.Fatalf("drain = %#v", report)
	}
	if report.Cursors[TriggerSourcePublications] == 0 {
		t.Fatalf("publication cursor did not advance: %#v", report.Cursors)
	}
	// The conversation had no commits: the pass derived nothing from them and
	// the imported fact is the view's, not the pass's. What the pass owes is
	// the generation switch, which is what makes the fact readable.
	active, found, err := fixture.facts.ActiveGeneration(ctx, fixture.scope, "conv-a")
	if err != nil || !found || active != "digest-test" {
		t.Fatalf("active generation = %q, %v, %v", active, found, err)
	}
}

// TestTriggerDiagnosticsReportsCursorsAndLease pins the reading a host gets:
// per-source cursor and pending work, and the claim stored for the scope.
func TestTriggerDiagnosticsReportsCursorsAndLease(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a"}
	})
	fixture.recordTurns(t, "conv-a")
	// A fact written through the view is the second source's work: the drain
	// has something pending in both.
	if _, err := fixture.facts.Add(ctx, factview.AddRequest{
		ID: "fact-imported", Generation: "digest-test", Scope: fixture.scope, ConversationID: "conv-a",
		Content:    coremessage.NewTextContent("imported from somewhere"),
		Provenance: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message-imported", Revision: "1"}},
	}); err != nil {
		t.Fatal(err)
	}

	report, err := fixture.trigger.Diagnostics(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sources) != 2 {
		t.Fatalf("sources = %#v", report.Sources)
	}
	for _, source := range report.Sources {
		if !source.Pending || source.Cursor != 0 {
			t.Fatalf("source %q before the drain = %#v", source.Source, source)
		}
	}
	// Two polls: the first derives the conversation and publishes its facts,
	// the second consumes that publication, and then the scope is quiet.
	for poll := 0; poll < 2; poll++ {
		if _, err := fixture.trigger.RunScope(ctx, fixture.scope); err != nil {
			t.Fatal(err)
		}
	}
	report, err = fixture.trigger.Diagnostics(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range report.Sources {
		if source.Pending {
			t.Fatalf("source %q still pending after the drain: %#v", source.Source, source)
		}
		if source.Cursor == 0 {
			t.Fatalf("source %q cursor after the drain = %d", source.Source, source.Cursor)
		}
	}
	if report.Lease == nil || report.Lease.Holder != "runner-a" || !report.Lease.Released {
		t.Fatalf("lease after the drain = %#v", report.Lease)
	}
}

// triggerSource picks one source's reading out of a diagnostics report.
func triggerSource(t *testing.T, report TriggerDiagnostics, source string) TriggerSourceDiagnostics {
	t.Helper()
	for _, entry := range report.Sources {
		if entry.Source == source {
			return entry
		}
	}
	t.Fatalf("source %q is not reported: %#v", source, report.Sources)
	return TriggerSourceDiagnostics{}
}

// TestTriggerJournalReplaysOneRecordPerCommit pins the journal's identity: a
// write path that retries -- the same commit, the same record -- lands one
// record, and a different record under the same key is a conflict rather than
// a duplicate the journal hides.
func TestTriggerJournalReplaysOneRecordPerCommit(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t)
	if err := fixture.journal.RecordTurn(ctx, fixture.scope, "conv-a", "commit-1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.RecordTurn(ctx, fixture.scope, "conv-a", "commit-1"); err != nil {
		t.Fatalf("replaying a record = %v", err)
	}
	entries, _, err := fixture.journal.Read(ctx, fixture.scope, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("journal holds %d records, want the one replayed", len(entries))
	}
	if entries[0].Record.Kind != TriggerKindTurn || entries[0].Record.ConversationID != "conv-a" {
		t.Fatalf("record = %#v", entries[0].Record)
	}
	err = fixture.journal.Record(ctx, fixture.scope, TriggerRecord{
		Kind: TriggerKindTurn, ConversationID: "conv-b", Ref: "commit:commit-1",
	})
	if err == nil {
		t.Fatal("a different record under a taken key was accepted")
	}
	if err := fixture.journal.Record(ctx, fixture.scope, TriggerRecord{
		Kind: TriggerKindTurn, ConversationID: "conv-b", Ref: "",
	}); err == nil {
		t.Fatal("a record with no reference was accepted")
	}
	if err := fixture.journal.Record(ctx, fixture.scope, TriggerRecord{
		Kind: "unknown", Ref: "x",
	}); err == nil {
		t.Fatal("a record of an unknown kind was accepted")
	}
	if err := fixture.journal.RecordDocument(ctx, fixture.scope, "document:d@1"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.Record(ctx, fixture.scope, TriggerRecord{
		Kind: TriggerKindDocument, ConversationID: "conv-a", Ref: "document:d@2",
	}); err == nil {
		t.Fatal("a document record naming a conversation was accepted")
	}
}

// TestTriggerRejectsUnusableWiring pins the construction refusals.
func TestTriggerRejectsUnusableWiring(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a"}
	})
	cases := map[string]TriggerConfig{
		"no journal":      {Processor: fixture.processor, Leases: fixture.leases, Cursors: fixture.cursors},
		"no processor":    {Journal: fixture.journal, Leases: fixture.leases, Cursors: fixture.cursors},
		"no lease store":  {Journal: fixture.journal, Processor: fixture.processor, Cursors: fixture.cursors},
		"no cursor store": {Journal: fixture.journal, Processor: fixture.processor, Leases: fixture.leases},
		"bad holder": {
			Journal: fixture.journal, Processor: fixture.processor,
			Leases: fixture.leases, Cursors: fixture.cursors, Holder: " runner",
		},
		"negative ttl": {
			Journal: fixture.journal, Processor: fixture.processor,
			Leases: fixture.leases, Cursors: fixture.cursors, LeaseTTL: -time.Second,
		},
		"negative batch": {
			Journal: fixture.journal, Processor: fixture.processor,
			Leases: fixture.leases, Cursors: fixture.cursors, Batch: -1,
		},
	}
	for name, config := range cases {
		if _, err := NewTrigger(config); err == nil {
			t.Fatalf("%s: construction was accepted", name)
		}
	}
	trigger, err := NewTrigger(TriggerConfig{
		Journal: fixture.journal, Processor: fixture.processor,
		Leases: fixture.leases, Cursors: fixture.cursors,
	})
	if err != nil {
		t.Fatal(err)
	}
	if trigger.Holder() == "" {
		t.Fatal("a trigger without a holder minted none")
	}
	if _, err := fixture.trigger.RunScope(nil, fixture.scope); err == nil {
		t.Fatal("a drain without a context was accepted")
	}
	if _, err := fixture.trigger.RunScope(ctx, corememory.Scope{}); err == nil {
		t.Fatal("a drain of an invalid scope was accepted")
	}
	var missing *Trigger
	if _, err := missing.RunScope(ctx, fixture.scope); err == nil {
		t.Fatal("a drain by a nil trigger was accepted")
	}
	if err := fixture.journal.RecordTurn(ctx, fixture.scope, "  ", "commit-1"); err == nil {
		t.Fatal("a turn record with no conversation was accepted")
	}
}

// TestTriggerKeepsAPassOutOfARebuildingScope pins that a scope whose lanes are
// being rebuilt is left for the next poll: a pass would index beside the
// rebuild.
func TestTriggerKeepsAPassOutOfARebuildingScope(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a"}
	})
	fixture.recordTurns(t, "conv-a")
	rebuilt, err := fixture.processor.BeginRepair(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	report, err := fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Repairing || report.Derived != 0 || report.Consumed != 0 {
		t.Fatalf("drain during a rebuild = %#v", report)
	}
	rebuilt()
	report, err = fixture.trigger.RunScope(ctx, fixture.scope)
	if err != nil {
		t.Fatal(err)
	}
	if report.Derived != 1 || report.Consumed != 1 {
		t.Fatalf("drain after the rebuild = %#v", report)
	}
}

// TestTriggerStatsCountWhatTheDrainDid pins the counters a host reads.
func TestTriggerStatsCountWhatTheDrainDid(t *testing.T) {
	ctx := context.Background()
	fixture := newTriggerFixture(t, func(config *triggerFixtureConfig) {
		config.conversations = []string{"conv-a", "conv-b"}
	})
	fixture.recordTurns(t, "conv-a", "conv-b")
	if _, err := fixture.trigger.RunScope(ctx, fixture.scope); err != nil {
		t.Fatal(err)
	}
	stats := fixture.trigger.Stats()
	if stats.Runs != 1 || stats.ScopesLeased != 1 || stats.RecordsRead != 2 ||
		stats.Consumed != 2 || stats.Conversations != 2 || stats.Derived != 2 {
		t.Fatalf("stats = %#v", stats)
	}
	if stats.LastRunAt.IsZero() || stats.LastError != "" {
		t.Fatalf("stats = %#v", stats)
	}
}

// TestTriggerKeepsTheErrorItHit pins that a failure a drain reported is also
// readable where a host looks for it.
func TestTriggerKeepsTheErrorItHit(t *testing.T) {
	fixture := newTriggerFixture(t)
	fixture.trigger.RecordError(errors.New("boom"))
	if stats := fixture.trigger.Stats(); stats.LastError != "boom" {
		t.Fatalf("stats = %#v", stats)
	}
	// Recording nothing is a no-op: the field holds the last failure there
	// was, so a caller can report an error without having to word one.
	fixture.trigger.RecordError(nil)
	if stats := fixture.trigger.Stats(); stats.LastError != "boom" {
		t.Fatalf("recording no error changed the reading: %#v", stats)
	}
	fixture.trigger.RecordError(errors.New("later"))
	if stats := fixture.trigger.Stats(); stats.LastError != "later" {
		t.Fatalf("the latest failure did not replace the first: %#v", stats)
	}
}
