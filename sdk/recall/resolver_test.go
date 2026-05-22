package recall_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// fakeResolver is a deterministic [recall.UpdateResolver] used to test
// the Save -> resolver -> apply path without an LLM. It either runs a
// caller-provided function (for batch-aware tests) or returns a static
// canned set of actions.
type fakeResolver struct {
	fn      func(batch recall.ResolveBatch) ([]recall.ResolveAction, error)
	actions []recall.ResolveAction
	err     error

	calls      int
	lastBatch  recall.ResolveBatch
	gotBatches []recall.ResolveBatch
}

func (r *fakeResolver) Resolve(_ context.Context, batch recall.ResolveBatch) ([]recall.ResolveAction, error) {
	r.calls++
	r.lastBatch = batch
	r.gotBatches = append(r.gotBatches, batch)
	if r.err != nil {
		return nil, r.err
	}
	if r.fn != nil {
		return r.fn(batch)
	}
	return r.actions, nil
}

// saveText is a tiny helper that runs Save with a single user message
// and returns the resulting entry IDs in order.
func saveText(t *testing.T, m recall.Memory, scope recall.Scope, text string) []string {
	t.Helper()
	res, err := m.Save(context.Background(), scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: text}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.EntryIDs
}

// firstSourceID returns the EntryID of the first new fact the resolver
// saw — handy when wiring resolver actions that must reference a real
// new fact, otherwise the action is rejected as a hallucinated
// source_id.
func firstSourceID(t *testing.T, batch recall.ResolveBatch) string {
	t.Helper()
	if len(batch.NewFacts) == 0 {
		t.Fatalf("expected resolver batch to contain at least one new fact")
	}
	return batch.NewFacts[0].EntryID
}

func TestResolver_UpdateMarksSuperseded(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user owns a labrador named Lucky"}},
			{{Content: "user has a poodle named Max"}},
		},
	}
	resolver := &fakeResolver{}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithUpdateResolver(resolver, 5),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	firstIDs := saveText(t, m, scope, "I have a labrador named Lucky")
	// Wire the resolver to UPDATE the first entry; SourceID is filled
	// from the batch so the action survives the hallucination guard.
	resolver.fn = func(batch recall.ResolveBatch) ([]recall.ResolveAction, error) {
		return []recall.ResolveAction{{
			Op:       recall.OpUpdate,
			SourceID: firstSourceID(t, batch),
			TargetID: firstIDs[0],
		}}, nil
	}

	saveText(t, m, scope, "Actually it's a poodle named Max")
	if resolver.calls != 1 {
		t.Fatalf("resolver should be called once (second Save); got %d", resolver.calls)
	}
	doc, ok, _ := idx.Get(ctx, recall.NamespaceFor(scope), firstIDs[0])
	if !ok {
		t.Fatalf("first entry vanished")
	}
	if _, has := doc.Metadata["superseded_by"]; !has {
		t.Fatalf("first entry should be tagged superseded_by, metadata=%v", doc.Metadata)
	}
	if got, _ := doc.Metadata["tombstone"]; got == true {
		t.Fatalf("UPDATE must not set tombstone; got %v", got)
	}
}

func TestResolver_DeleteSetsTombstoneAndHidesFromRecall(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user has a dog called Lucky"}},
			{{Content: "user no longer has a pet"}},
		},
	}
	resolver := &fakeResolver{}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithUpdateResolver(resolver, 5),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	firstIDs := saveText(t, m, scope, "I have a dog called Lucky")
	resolver.fn = func(batch recall.ResolveBatch) ([]recall.ResolveAction, error) {
		return []recall.ResolveAction{{
			Op:       recall.OpDelete,
			SourceID: firstSourceID(t, batch),
			TargetID: firstIDs[0],
		}}, nil
	}
	saveText(t, m, scope, "Lucky passed away last week")

	doc, ok, _ := idx.Get(ctx, recall.NamespaceFor(scope), firstIDs[0])
	if !ok {
		t.Fatalf("DELETE must be soft (tombstone); doc was hard-removed")
	}
	if got := doc.Metadata["tombstone"]; got != true {
		t.Fatalf("tombstone=%v, want true", got)
	}
	hits, err := m.Recall(ctx, scope, recall.Request{Query: "Lucky", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Entry.ID == firstIDs[0] {
			t.Fatalf("Recall must hide tombstoned entry %q", firstIDs[0])
		}
	}
}

func TestResolver_NoopAndAddDoNotMutateStore(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user likes hiking"}},
			{{Content: "user enjoys cycling on weekends"}},
		},
	}
	resolver := &fakeResolver{}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithUpdateResolver(resolver, 5),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()
	firstIDs := saveText(t, m, scope, "I like hiking")
	resolver.fn = func(batch recall.ResolveBatch) ([]recall.ResolveAction, error) {
		// Mix of NOOP and ADD — neither should touch existing docs.
		return []recall.ResolveAction{
			{Op: recall.OpNoop, SourceID: firstSourceID(t, batch), TargetID: "irrelevant"},
			{Op: recall.OpAdd, SourceID: firstSourceID(t, batch)},
		}, nil
	}
	saveText(t, m, scope, "I also enjoy cycling on weekends")

	doc, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), firstIDs[0])
	if _, has := doc.Metadata["superseded_by"]; has {
		t.Fatalf("NOOP/ADD must not set superseded_by; got %v", doc.Metadata["superseded_by"])
	}
}

func TestResolver_SkipsFactsWithSlotFields(t *testing.T) {
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user lives in Guangzhou", Subject: "user", Predicate: "lives_in"}},
		},
	}
	resolver := &fakeResolver{}
	m, err := recall.New(memidx.New(),
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithUpdateResolver(resolver, 5),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	saveText(t, m, newScope(), "我住在广州")
	if resolver.calls != 0 {
		t.Fatalf("resolver must NOT be called for slot facts; got %d calls", resolver.calls)
	}
}

func TestResolver_ErrorIsSwallowed(t *testing.T) {
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user said something interesting"}},
			{{Content: "user said something else"}},
		},
	}
	resolver := &fakeResolver{err: errors.New("boom")}
	m, err := recall.New(memidx.New(),
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithUpdateResolver(resolver, 5),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()
	saveText(t, m, scope, "first")
	// Resolver fails — Save must still succeed.
	if _, err := m.Save(context.Background(), scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "second"}}},
	}); err != nil {
		t.Fatalf("Save must succeed even when resolver fails; got %v", err)
	}
}

// TestResolver_BatchSeesAllNewFacts asserts that when a single Save
// produces multiple non-slot facts the resolver receives ONE batch
// containing all of them — not one call per fact. This is the core
// behaviour change vs the per-fact resolver of the previous design.
func TestResolver_BatchSeesAllNewFacts(t *testing.T) {
	idx := memidx.New()
	// Seed an existing entry the second Save's resolver will see as a
	// candidate.
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user used to be married to Alice"}},
			// Second Save extracts TWO related facts in one batch.
			{
				{Content: "user divorced Alice"},
				{Content: "user is now married to Beth"},
			},
		},
	}
	resolver := &fakeResolver{}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithUpdateResolver(resolver, 5),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()
	saveText(t, m, scope, "I was married to Alice")

	resolver.fn = func(batch recall.ResolveBatch) ([]recall.ResolveAction, error) {
		// Sanity-check: the resolver sees BOTH new facts together.
		if len(batch.NewFacts) != 2 {
			t.Errorf("expected batch with 2 new facts, got %d", len(batch.NewFacts))
		}
		return nil, nil
	}
	saveText(t, m, scope, "I divorced Alice. I'm now married to Beth.")

	if resolver.calls != 1 {
		t.Fatalf("resolver must be called exactly once per Save; got %d", resolver.calls)
	}
	if got := len(resolver.lastBatch.NewFacts); got != 2 {
		t.Fatalf("last batch new fact count = %d, want 2", got)
	}
}

// TestResolver_HallucinatedSourceIDIsRejected verifies the defensive
// guard in applyResolverActions: actions whose source_id does not
// match any new fact in the batch are dropped with no side effect on
// the store.
func TestResolver_HallucinatedSourceIDIsRejected(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user enjoys reading"}},
			{{Content: "user enjoys writing"}},
		},
	}
	resolver := &fakeResolver{}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithUpdateResolver(resolver, 5),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()
	firstIDs := saveText(t, m, scope, "I enjoy reading")
	resolver.fn = func(batch recall.ResolveBatch) ([]recall.ResolveAction, error) {
		return []recall.ResolveAction{{
			Op:       recall.OpUpdate,
			SourceID: "fabricated-id-not-in-batch",
			TargetID: firstIDs[0],
		}}, nil
	}
	saveText(t, m, scope, "I also enjoy writing")
	doc, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), firstIDs[0])
	if _, has := doc.Metadata["superseded_by"]; has {
		t.Fatalf("hallucinated source_id must not be applied; metadata=%v", doc.Metadata)
	}
}
