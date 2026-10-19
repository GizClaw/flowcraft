package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/lines/chat"
	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// supersedeSettings derives facts from the injected deriver, keeps summaries
// out of the way, and leaves the maintenance interval at zero: the test calls
// RunOnce and Maintain itself.
const supersedeSettings = `{
  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
  "fact": {"strategy": "none"},
  "summary": {"disabled": true},
  "interval": "0"
}`

// factTuple is one fact a turn derives: the phrase of the turn it comes from,
// the text the answer model reads, the relation the extractor would have
// written beside it, and the turn's date.
type factTuple struct {
	turn      string
	text      string
	entities  []string
	predicate string
	detail    string
	at        time.Time
}

// tupleDeriver derives the fact its table holds for a turn, and nothing for any
// other turn. It stands in for a rich extractor: the predicate and the turn's
// own time expression are what the maintenance pass reads, and a fixture that
// had to invent them from a model would be testing the model instead.
type tupleDeriver struct {
	tuples []factTuple
}

func (deriver tupleDeriver) Derive(_ context.Context, input component.Artifact) ([]component.Artifact, error) {
	for _, tuple := range deriver.tuples {
		if !strings.Contains(input.Content.Text(), tuple.turn) {
			continue
		}
		hash := factview.CanonicalHash(tuple.text)
		entities, err := json.Marshal(tuple.entities)
		if err != nil {
			return nil, err
		}
		metadata := corememory.Metadata{
			"canonical_hash":      hash,
			"entities":            string(entities),
			"event_time":          tuple.at.Format(time.RFC3339Nano),
			"predicate":           tuple.predicate,
			"temporal_detail":     tuple.detail,
			"source_digest":       factview.ComputeSourceDigest(input.Sources),
			"transform_signature": "tuple-derive-v1",
		}
		return []component.Artifact{{
			Kind:     chat.KindFact,
			ID:       "fact-" + strings.ReplaceAll(hash, ":", "-"),
			Content:  coremessage.NewTextContent(tuple.text),
			Sources:  append([]corememory.SourceRef(nil), input.Sources...),
			Metadata: metadata,
		}}, nil
	}
	return nil, nil
}

// TestMaintenanceKeepsASupersededFactAnswerable is the O10 acceptance at the
// assembly surface, and it is where the temporal lane's safety lives: a pass
// that judges an update by relation reduces the older fact's score and records
// the window it was current over, and does nothing else -- the older fact keeps
// its text, its date and its place in the read path, so a question about the
// older state is still answered from it. Nothing in this PR hides a fact, and
// nothing rewrites one.
func TestMaintenanceKeepsASupersededFactAnswerable(t *testing.T) {
	ctx := context.Background()
	olderAt := testNow.AddDate(0, 0, -13)
	newerAt := testNow.AddDate(0, 0, -8)
	deriver := tupleDeriver{tuples: []factTuple{
		{
			turn:      "we compared notes about the old flat",
			text:      "Caroline lives in Berlin, in the flat above the bakery she has rented since 2019.",
			entities:  []string{"Caroline", "Berlin"},
			predicate: "lives in", detail: "since 2019", at: olderAt,
		},
		{
			turn:      "and then we heard about the move",
			text:      "Caroline moved her whole household to Lisbon in the autumn of 2024.",
			entities:  []string{"Caroline", "Lisbon"},
			predicate: "moved to", detail: "in the autumn of 2024", at: newerAt,
		},
	}}
	assembly, ws := newTestAssemblyWith(t, supersedeSettings, WithDeriver(deriver))
	for index, text := range []string{"we compared notes about the old flat", "and then we heard about the move"} {
		turn := corememory.Turn{
			Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-" + string(rune('1'+index)),
			Messages: []coremessage.Message{textMessage(coremessage.RoleUser, text)},
		}
		if err := assembly.CommitTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
		if err := assembly.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	facts := listedFacts(t, assembly)
	older, hasOlder := factWithPredicate(facts, "lives in")
	newer, hasNewer := factWithPredicate(facts, "moved to")
	if !hasOlder || !hasNewer {
		t.Fatalf("facts = %#v, want the two derived tuples", facts)
	}

	plan, err := assembly.Maintain(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 1 {
		t.Fatalf("supersedes = %#v", plan.Supersedes)
	}
	finding := plan.Supersedes[0]
	if finding.FactID != older.ID || finding.SupersededBy != newer.ID ||
		finding.Reason != maintain.ReasonRelation || finding.Relation != "residence" {
		t.Fatalf("finding = %+v", finding)
	}
	if !finding.ValidAt.Equal(olderAt) || !finding.InvalidAt.Equal(newerAt) ||
		finding.ValidDetail != "since 2019" || finding.InvalidDetail != "in the autumn of 2024" {
		t.Fatalf("window = %v..%v (%q..%q)", finding.ValidAt, finding.InvalidAt,
			finding.ValidDetail, finding.InvalidDetail)
	}

	// The pass wrote an overlay and nothing else: the fact a temporal question
	// is answered from is still stored with its own text, date and expression.
	after := listedFacts(t, assembly)
	storedOlder, ok := factByID(after, older.ID)
	if !ok {
		t.Fatalf("the superseded fact is gone from the view: %#v", after)
	}
	if storedOlder.Text != older.Text || !storedOlder.EventTime.Equal(older.EventTime) ||
		storedOlder.TemporalDetail != older.TemporalDetail || storedOlder.CanonicalHash != older.CanonicalHash {
		t.Fatalf("the pass rewrote the fact: %#v", storedOlder)
	}

	// The window is what a host reads back, and the factor is a penalty rather
	// than a removal -- 0 would be the fact leaving the read path, which a
	// supersede is not.
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := maintain.NewStore(kv)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Load(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	identity := corememory.ContextAddress{
		Kind: corememory.ContextFact, ConversationID: "conv-1", ItemID: older.ID,
	}.Identity(testScope())
	entry, found := entries[identity]
	if !found {
		t.Fatalf("no overlay entry for the superseded fact: %#v", entries)
	}
	if entry.SupersededBy != newer.ID || entry.Relation != "residence" ||
		entry.Factor <= 0 || entry.Factor >= 1 ||
		!entry.ValidAt.Equal(olderAt) || !entry.InvalidAt.Equal(newerAt) {
		t.Fatalf("entry = %#v", entry)
	}

	// And the read path still serves it: the question the older fact answers
	// keeps its evidence, with the date the text states.
	served := servedFactIDs(t, assembly, "flat above the bakery")
	if !containsString(served, older.ID) {
		t.Fatalf("served facts = %#v, want the superseded fact among them", served)
	}
}

func listedFacts(t *testing.T, assembly *Assembly) []factview.Fact {
	t.Helper()
	facts, err := assembly.Facts().List(context.Background(), testScope(), "conv-1", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func factWithPredicate(facts []factview.Fact, predicate string) (factview.Fact, bool) {
	for _, fact := range facts {
		if fact.Predicate == predicate {
			return fact, true
		}
	}
	return factview.Fact{}, false
}

func factByID(facts []factview.Fact, id string) (factview.Fact, bool) {
	for _, fact := range facts {
		if fact.ID == id {
			return fact, true
		}
	}
	return factview.Fact{}, false
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
