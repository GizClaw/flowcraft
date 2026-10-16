package memory

import (
	"context"
	"strings"
	"testing"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	"github.com/GizClaw/flowcraft/backends/memory/worker"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// triggerSettings is the deployment an O7b test asks for: the sweep turned off
// (interval "0") and the trigger runner turned off (derive.trigger.interval
// "0"), so every pass in the fixture is one a test asked for by hand. The
// journal is built either way -- the write paths record the same work whether a
// runner polls or not -- which is what half of these tests are about.
const triggerSettings = `{
  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
  "fact": {"strategy": "none"},
  "summary": {"disabled": true},
  "interval": "0",
  "derive": {"trigger": {"interval": "0"}}
}`

// TestAssemblyTriggerDerivesACommittedTurn pins the fast path end to end: a
// committed turn records the work before the write returns, the trigger's drain
// derives the conversation the record names, and the derived fact is
// retrievable -- with the sweep never running.
func TestAssemblyTriggerDerivesACommittedTurn(t *testing.T) {
	assembly, _ := newTestAssemblyWith(t, triggerSettings, WithDeriver(fakeDeriver{}))
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "we talked about beverages")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	// The write path owed a derivation, and said so durably: the record is
	// there before the turn call returned.
	entries, cursor, err := assembly.journal.Read(ctx, testScope(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || cursor != entries[0].Seq {
		t.Fatalf("journal after the commit = %#v (cursor %d), want the turn's record", entries, cursor)
	}
	record := entries[0].Record
	if record.Kind != worker.TriggerKindTurn || record.ConversationID != "conv-1" ||
		!strings.HasPrefix(record.Ref, "commit:") || record.Scope() != testScope() {
		t.Fatalf("record = %#v", record)
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	replayed, _, err := assembly.journal.Read(ctx, testScope(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 {
		t.Fatalf("a replayed turn landed %d records, want the one it already had", len(replayed))
	}
	// Nothing is retrievable yet: the sweep is off and the trigger has not run.
	facts, err := assembly.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("facts before the drain = %#v", facts)
	}
	reports, err := assembly.RunTriggerOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !reports[0].Held || reports[0].Derived != 1 || reports[0].Consumed != 1 {
		t.Fatalf("drain reports = %#v", reports)
	}
	if ids := reports[0].ConversationIDs; len(ids) != 1 || ids[0] != "conv-1" {
		t.Fatalf("the drain named the conversations it derived as %#v", ids)
	}
	facts, err = assembly.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Content.Text() != "Alice likes tea" {
		t.Fatalf("facts after the drain = %#v", facts)
	}
	// Retrievable through the ordinary read path, with the sweep off: the
	// long-term item is there because the trigger ran the pass.
	result, err := assembly.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "tea",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Items {
		if item.Content.Text() == "Alice likes tea" && item.SourceClass == corememory.ContextSourceLongTerm {
			found = true
		}
	}
	if !found {
		t.Fatalf("the trigger's fact is not recallable: %#v", result.Items)
	}
}

// TestAssemblyTriggerSettlesAndReportsItsCursors pins the reading a host gets
// and the bound on the work: the drain's own fact publications are the next
// poll's work, the polls settle, and diagnostics reports the cursor, the lease
// and the counters.
func TestAssemblyTriggerSettlesAndReportsItsCursors(t *testing.T) {
	assembly, _ := newTestAssemblyWith(t, triggerSettings, WithDeriver(fakeDeriver{}))
	ctx := context.Background()
	if err := assembly.CommitTurn(ctx, corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "we talked about beverages")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := assembly.RunTriggerOnce(ctx); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := assembly.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.Trigger == nil {
		t.Fatal("an assembly with a trigger reports no trigger counters")
	}
	if diagnostics.Trigger.Runs != 1 || diagnostics.Trigger.Derived != 1 {
		t.Fatalf("trigger counters = %#v", diagnostics.Trigger)
	}
	// One poll ran, and the fact it derived was published by the view, so the
	// scope still has work in its publication source.
	if len(diagnostics.Scopes) != 1 || diagnostics.Scopes[0].Trigger == nil {
		t.Fatalf("scope diagnostics = %#v", diagnostics.Scopes)
	}
	reading := *diagnostics.Scopes[0].Trigger
	if reading.Lease == nil || reading.Lease.Holder == "" || !reading.Lease.Released {
		t.Fatalf("lease after the drain = %#v", reading.Lease)
	}
	journal := triggerReading(t, reading, "journal")
	if journal.Cursor == 0 || journal.Pending {
		t.Fatalf("journal reading = %#v", journal)
	}
	// The polls settle: the pass the publication asked for derives nothing and
	// publishes nothing.
	polls := 0
	for ; polls < 5; polls++ {
		reports, err := assembly.RunTriggerOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		records := 0
		for _, report := range reports {
			records += report.Records
		}
		if records == 0 {
			break
		}
	}
	if polls != 1 {
		t.Fatalf("the polls settled after %d polls with work, want the one the drain caused", polls)
	}
	facts, err := assembly.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts after the polls settled = %#v", facts)
	}
}

// TestAssemblyTriggerRunsTheDocumentPass pins the scope half of the drain: a
// document put through the assembly records its version, and the drain runs the
// document pass for it, which is what makes the document's chunks recallable
// without the sweep.
func TestAssemblyTriggerRunsTheDocumentPass(t *testing.T) {
	settings := `{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "chunk": {"max_runes": 64, "overlap_runes": 8},
	  "interval": "0",
	  "derive": {"trigger": {"interval": "0"}}
	}`
	assembly, _ := newTestAssemblyWith(t, settings)
	ctx := context.Background()
	if err := assembly.PutDocument(ctx, corememory.Document{
		Scope: testScope(), DatasetID: "docs", DocumentID: "doc-1", IdempotencyKey: "rev-1",
		Content: coremessage.Content{Parts: []coremessage.Part{
			coremessage.TextPart{Text: "The quarterly report covers espresso brewing techniques in detail."},
		}},
		Provenance: []corememory.SourceRef{{Kind: corememory.SourceExternal, ID: "uri:report"}},
	}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := assembly.journal.Read(ctx, testScope(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Record.Kind != worker.TriggerKindDocument {
		t.Fatalf("journal after the put = %#v", entries)
	}
	if ref := entries[0].Record.Ref; ref != "document:docs/doc-1@1" {
		t.Fatalf("document record ref = %q", ref)
	}
	reports, err := assembly.RunTriggerOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Documents != 1 || reports[0].Consumed != 1 {
		t.Fatalf("drain reports = %#v", reports)
	}
	result, err := assembly.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), Query: "espresso", DatasetIDs: []string{"docs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Items {
		if item.Kind == corememory.ContextDocumentChunk && strings.Contains(item.Content.Text(), "espresso") {
			found = true
		}
	}
	if !found {
		t.Fatalf("document chunk missing from context items = %#v", result.Items)
	}
}

// TestARecordThatCannotLandIsADiagnosticNotAFailedWrite pins the rule the write
// paths rely on: recording the work is best effort. The write it belongs to is
// already durable, so a record the journal refuses is recorded where a host
// reads diagnostics rather than reported as a failure of the write.
func TestARecordThatCannotLandIsADiagnosticNotAFailedWrite(t *testing.T) {
	assembly, _ := newTestAssemblyWith(t, triggerSettings, WithDeriver(fakeDeriver{}))
	ctx := context.Background()
	// A record with no reference is one the journal refuses.
	assembly.recordWork(ctx, testScope(), worker.TriggerRecord{
		Kind: worker.TriggerKindTurn, ConversationID: "conv-1",
	})
	if stats := assembly.trigger.Stats(); stats.LastError == "" {
		t.Fatal("the refused record was dropped instead of recorded")
	}
	entries, _, err := assembly.journal.Read(ctx, testScope(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the refused record was stored anyway: %#v", entries)
	}
	if reports, err := assembly.RunTriggerOnce(ctx); err != nil {
		t.Fatal(err)
	} else if len(reports) != 1 || !reports[0].Held || reports[0].Records != 0 || reports[0].Derived != 0 {
		t.Fatalf("drain of a scope with no work = %#v", reports)
	}
}

// triggerReading picks one source's reading out of a scope's trigger report.
func triggerReading(t *testing.T, report worker.TriggerDiagnostics, source string) worker.TriggerSourceDiagnostics {
	t.Helper()
	for _, entry := range report.Sources {
		if entry.Source == source {
			return entry
		}
	}
	t.Fatalf("source %q is not reported: %#v", source, report.Sources)
	return worker.TriggerSourceDiagnostics{}
}
