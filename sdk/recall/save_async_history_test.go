package recall_test

import (
	"context"
	"strings"
	"testing"
	"time"

	memidx "github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall"
)

// TestSaveAsync_HonoursHistoryStore pins the fix for issue #149.
// Pre-fix, the async worker's handleJob skipped both halves of
// WithHistoryStore: it did not feed previous-batch RecentMessages
// to the extractor, and it did not append the current batch to
// the history store after a successful upsert. As a result, two
// back-to-back SaveAsync calls on the same scope produced the
// same "first save sees empty recent" state on both calls.
//
// This is the SaveAsync mirror of TestSaveInjectsRecentTurnsFromHistory
// in recent_turns_test.go.
func TestSaveAsync_HonoursHistoryStore(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	hs := history.NewInMemoryStore()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "User mentioned a gym", Categories: []string{"events"}}},
			{{Content: "User goes to the gym on weekends", Categories: []string{"events"}}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithHistoryStore(hs, 5),
		recall.WithAsyncWorkers(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	jc, ok := m.(recall.JobController)
	if !ok {
		t.Fatalf("recall.Memory does not implement JobController")
	}

	id1, err := m.SaveAsync(ctx, scope, []llm.Message{
		msgUser("I joined a new gym yesterday."),
		msgAssistant("Nice — which one?"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jc.AwaitJob(ctx, id1, 5*time.Second); err != nil {
		t.Fatalf("await1: %v", err)
	}

	id2, err := m.SaveAsync(ctx, scope, []llm.Message{
		msgUser("I go there every Saturday morning."),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jc.AwaitJob(ctx, id2, 5*time.Second); err != nil {
		t.Fatalf("await2: %v", err)
	}

	if ex.calls != 2 {
		t.Fatalf("extractor calls=%d want 2", ex.calls)
	}
	if len(ex.gotRecent[0]) != 0 {
		t.Fatalf("first SaveAsync should see empty RecentMessages, got %+v", ex.gotRecent[0])
	}
	if len(ex.gotRecent[1]) != 2 {
		t.Fatalf("second SaveAsync should see 2 recent messages, got %d: %+v", len(ex.gotRecent[1]), ex.gotRecent[1])
	}
	if !strings.Contains(ex.gotRecent[1][0].Content(), "joined a new gym") {
		t.Fatalf("oldest recent message wrong: %q", ex.gotRecent[1][0].Content())
	}
	if !strings.Contains(ex.gotRecent[1][1].Content(), "which one") {
		t.Fatalf("newest recent message wrong: %q", ex.gotRecent[1][1].Content())
	}
}
