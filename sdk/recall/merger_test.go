package recall_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestSaveDedupsSameFactAcrossDifferentMessages(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "User likes matcha", Entities: []string{"matcha"}}},
			{{Content: "User likes matcha", Entities: []string{"matcha"}}},
		},
	}
	m, err := recall.New(idx, recall.WithRequireUserID(), recall.WithExtractor(ex))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	r1, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "Remember that I like matcha."}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "Also note that matcha is still my favorite drink."}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if r1.EntryIDs[0] != r2.EntryIDs[0] {
		t.Fatalf("expected md5 dedup to return existing id, got %q and %q", r1.EntryIDs[0], r2.EntryIDs[0])
	}
	hits, err := m.Recall(ctx, scope, recall.Request{Query: "matcha", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected one deduped hit, got %+v", hits)
	}
}

func TestSaveSoftMergeMarksSupersededNeighbour(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	now := time.Now()
	oldFact := "Alice prefers pour-over coffee"
	newFact := "Alice now prefers pour over coffee at work"
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: oldFact, Entities: []string{"Alice", "Coffee"}}},
			{{Content: newFact, Entities: []string{"coffee", "alice"}}},
		},
	}
	emb := &mapEmbedder{
		vectors: map[string][]float32{
			oldFact: {1, 0},
			newFact: {1, 0},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithEmbedder(emb),
		recall.WithClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	first, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "Alice prefers pour-over coffee."}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	second, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "Alice now prefers pour over coffee at work."}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	doc, ok, err := idx.Get(ctx, recall.NamespaceFor(scope), first.EntryIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("missing original doc %q", first.EntryIDs[0])
	}
	if got := doc.Metadata["superseded_by"]; got != second.EntryIDs[0] {
		t.Fatalf("superseded_by=%v, want %q", got, second.EntryIDs[0])
	}
	if got := doc.Metadata["superseded_at"]; got != now.UnixMilli() {
		t.Fatalf("superseded_at=%v, want %d", got, now.UnixMilli())
	}
}
