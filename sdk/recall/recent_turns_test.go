package recall_test

import (
	"context"
	"strings"
	"testing"

	memidx "github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
)

func msgUser(text string) llm.Message {
	return llm.Message{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: text}}}
}

func msgAssistant(text string) llm.Message {
	return llm.Message{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartText, Text: text}}}
}

// TestSaveInjectsRecentTurnsFromHistory wires the recall layer to
// the existing sdk/history.Store primitive (instead of a bespoke
// MessageBuffer) and asserts:
//  1. the FIRST Save sees no recent turns (history is empty);
//  2. the SECOND Save's extractor receives the messages of the
//     FIRST Save, in chronological order, via RecentMessages;
//  3. the current batch never bleeds into RecentMessages — it
//     belongs to CONVERSATION, not RECENT TURNS.
func TestSaveInjectsRecentTurnsFromHistory(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "User mentioned a gym", Categories: []string{"events"}}},
			{{Content: "User goes to the gym on weekends", Categories: []string{"events"}}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithRecentTurns(5),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()

	if _, err := m.Save(ctx, scope, []llm.Message{
		msgUser("I joined a new gym yesterday."),
		msgAssistant("Nice — which one?"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Save(ctx, scope, []llm.Message{
		msgUser("I go there every Saturday morning."),
	}); err != nil {
		t.Fatal(err)
	}

	if ex.calls != 2 {
		t.Fatalf("extractor calls=%d want 2", ex.calls)
	}
	if len(ex.gotRecent[0]) != 0 {
		t.Fatalf("first save should see empty RecentMessages, got %+v", ex.gotRecent[0])
	}
	if len(ex.gotRecent[1]) != 2 {
		t.Fatalf("second save should see 2 recent messages, got %d: %+v", len(ex.gotRecent[1]), ex.gotRecent[1])
	}
	if !strings.Contains(ex.gotRecent[1][0].Content(), "joined a new gym") {
		t.Fatalf("oldest recent message wrong: %q", ex.gotRecent[1][0].Content())
	}
	if !strings.Contains(ex.gotRecent[1][1].Content(), "which one") {
		t.Fatalf("newest recent message wrong: %q", ex.gotRecent[1][1].Content())
	}
	for _, msg := range ex.gotRecent[1] {
		if strings.Contains(msg.Content(), "every Saturday") {
			t.Fatalf("current batch leaked into RecentMessages: %q", msg.Content())
		}
	}
}

// TestSaveSkipsRecentTurnsWhenDisabled guarantees the opt-in
// semantics: without WithRecentTurns/WithHistoryStore, no Save
// should ever populate ExtractOptions.RecentMessages.
func TestSaveSkipsRecentTurnsWhenDisabled(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "x"}},
			{{Content: "y"}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	for i := 0; i < 2; i++ {
		if _, err := m.Save(ctx, scope, []llm.Message{msgUser("hello")}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	for i, r := range ex.gotRecent {
		if len(r) != 0 {
			t.Fatalf("save %d unexpectedly received recent turns: %+v", i, r)
		}
	}
}

// TestSaveWithCustomHistoryStore exercises the WithHistoryStore
// path: a caller-supplied store (e.g. history.FileStore in
// production) is honoured end-to-end. We use a counting wrapper to
// also assert that the RecentReader / MessageAppender optional
// fast-paths are taken when the store implements them.
func TestSaveWithCustomHistoryStore(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "f1"}},
			{{Content: "f2"}},
		},
	}
	store := &countingHistoryStore{InMemoryStore: history.NewInMemoryStore()}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithHistoryStore(store, 4),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	if _, err := m.Save(ctx, scope, []llm.Message{msgUser("batch-1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(ctx, scope, []llm.Message{msgUser("batch-2")}); err != nil {
		t.Fatal(err)
	}

	if store.appendCalls != 2 {
		t.Fatalf("AppendMessages calls=%d want 2 (MessageAppender fast-path missed)", store.appendCalls)
	}
	if store.recentCalls != 2 {
		t.Fatalf("GetRecentMessages calls=%d want 2 (RecentReader fast-path missed)", store.recentCalls)
	}

	// The buffer must reflect both batches in chronological order.
	all, err := store.GetMessages(ctx, recall.NamespaceFor(scope))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Content() != "batch-1" || all[1].Content() != "batch-2" {
		t.Fatalf("history buffer state wrong: %+v", all)
	}
}

// countingHistoryStore wraps an InMemoryStore and counts how often
// the optional RecentReader / MessageAppender fast-paths are taken.
// The wrapper provides the optional fast-path interfaces itself
// (the base InMemoryStore only implements the Store contract), so
// asserting the call counts here also asserts that the recall
// layer's interface-detection logic dispatches correctly.
type countingHistoryStore struct {
	*history.InMemoryStore
	recentCalls int
	appendCalls int
}

func (s *countingHistoryStore) GetRecentMessages(ctx context.Context, convID string, k int) ([]model.Message, error) {
	s.recentCalls++
	all, err := s.InMemoryStore.GetMessages(ctx, convID)
	if err != nil {
		return nil, err
	}
	if k > 0 && k < len(all) {
		all = all[len(all)-k:]
	}
	return all, nil
}

func (s *countingHistoryStore) AppendMessages(ctx context.Context, convID string, msgs []model.Message) error {
	s.appendCalls++
	existing, err := s.InMemoryStore.GetMessages(ctx, convID)
	if err != nil {
		return err
	}
	return s.InMemoryStore.SaveMessages(ctx, convID, append(existing, msgs...))
}
