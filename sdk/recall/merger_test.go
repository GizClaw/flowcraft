package recall_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
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
	// Wrap the test clock in atomic.Pointer so the recall worker
	// goroutine (which calls the clock asynchronously while it drains
	// the save queue) and the test goroutine (which advances time
	// between saves) cannot race on the shared `now`. The previous
	// closure-over-local-variable form tripped -race in CI.
	var clockHolder atomic.Pointer[time.Time]
	setNow := func(t time.Time) { clockHolder.Store(&t) }
	getNow := func() time.Time { return *clockHolder.Load() }
	setNow(time.Now())
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
		recall.WithClock(getNow),
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
	setNow(getNow().Add(time.Minute))
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
	if got := doc.Metadata["superseded_at"]; got != getNow().UnixMilli() {
		t.Fatalf("superseded_at=%v, want %d", got, getNow().UnixMilli())
	}
}

// TestSlotSupersede_CityChange asserts the deterministic slot channel
// tags an older lives_in entry as superseded when the new fact carries
// the same (subject, predicate) tuple, even though the entities differ
// ("Guangzhou" vs "Shanghai") — the case the legacy vector+entity
// channel cannot handle.
func TestSlotSupersede_CityChange(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	var clockHolder atomic.Pointer[time.Time]
	setNow := func(t time.Time) { clockHolder.Store(&t) }
	getNow := func() time.Time { return *clockHolder.Load() }
	setNow(time.Now())
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user lives in Guangzhou", Subject: "user", Predicate: "lives_in", Entities: []string{"Guangzhou"}}},
			{{Content: "user lives in Shanghai", Subject: "user", Predicate: "lives_in", Entities: []string{"Shanghai"}}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithClock(getNow),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	first, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "我住在广州"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	setNow(getNow().Add(time.Hour))
	second, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "我搬到上海了"}}},
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
	// Slot key was recorded so SlotCollapse / future writes can target it.
	if got := doc.Metadata["slot_key"]; got != "user|lives_in" {
		t.Fatalf("slot_key=%v, want %q", got, "user|lives_in")
	}
	newDoc, _, err := idx.Get(ctx, recall.NamespaceFor(scope), second.EntryIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := newDoc.Metadata["slot_key"]; got != "user|lives_in" {
		t.Fatalf("new doc slot_key=%v, want %q", got, "user|lives_in")
	}
	if _, has := newDoc.Metadata["superseded_by"]; has {
		t.Fatalf("new doc should not be superseded; got %v", newDoc.Metadata["superseded_by"])
	}
}

// TestSlotSupersede_ChainedUpdates walks three sequential updates on
// the same slot and checks every prior entry points at the latest one.
// The middle entry must NOT be re-superseded once the third write
// arrives — supersedeBySlot intentionally skips entries that already
// carry superseded_by so chains stay sparse.
func TestSlotSupersede_ChainedUpdates(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	var clockHolder atomic.Pointer[time.Time]
	setNow := func(t time.Time) { clockHolder.Store(&t) }
	getNow := func() time.Time { return *clockHolder.Load() }
	setNow(time.Now())
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user lives in Guangzhou", Subject: "user", Predicate: "lives_in"}},
			{{Content: "user lives in Shanghai", Subject: "user", Predicate: "lives_in"}},
			{{Content: "user lives in Beijing", Subject: "user", Predicate: "lives_in"}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithClock(getNow),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	mkSave := func(text string) string {
		res, err := m.Save(ctx, scope, []llm.Message{
			{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: text}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		setNow(getNow().Add(time.Hour))
		return res.EntryIDs[0]
	}
	idGZ := mkSave("我住在广州")
	idSH := mkSave("我搬到上海了")
	idBJ := mkSave("我又搬到北京了")

	gz, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), idGZ)
	sh, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), idSH)
	bj, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), idBJ)

	// Guangzhou was superseded by the FIRST update (Shanghai) and is
	// then skipped by Beijing's pass because it already carries
	// superseded_by — see supersedeBySlot's "already pointed somewhere"
	// short-circuit.
	if got := gz.Metadata["superseded_by"]; got != idSH {
		t.Fatalf("Guangzhou.superseded_by=%v, want %q", got, idSH)
	}
	if got := sh.Metadata["superseded_by"]; got != idBJ {
		t.Fatalf("Shanghai.superseded_by=%v, want %q", got, idBJ)
	}
	if _, has := bj.Metadata["superseded_by"]; has {
		t.Fatalf("Beijing should be the active entry; got superseded_by=%v", bj.Metadata["superseded_by"])
	}
}

// TestSlotSupersede_NoSlotFallsBackToVectorEntity asserts the dispatch
// in supersedeNeighbours: if Subject/Predicate are empty the legacy
// vector+entity path still runs (and behaves as before), and if they
// are present the vector path is bypassed entirely (no embedder call
// is required).
func TestSlotSupersede_NoSlotFallsBackToVectorEntity(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			// Slot fact, no embedder configured — must still supersede.
			{{Content: "user lives in Guangzhou", Subject: "user", Predicate: "lives_in"}},
			{{Content: "user lives in Shanghai", Subject: "user", Predicate: "lives_in"}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		// Note: no WithEmbedder — slot channel must work without one.
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()
	first, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "我住广州"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "我搬上海"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), first.EntryIDs[0])
	if got := doc.Metadata["superseded_by"]; got != second.EntryIDs[0] {
		t.Fatalf("superseded_by=%v, want %q (slot channel must run without embedder)", got, second.EntryIDs[0])
	}
}

// TestSlotChannel_KeptWhenWithoutSoftMerge asserts the M3 fix:
// WithoutSoftMerge silences the VECTOR + entity supersede channel
// only — the deterministic SLOT channel keeps running for facts
// that carry both Subject and Predicate. This is the user-visible
// contract change separating "noisy soft merge" from "deterministic
// slot rewrite".
func TestSlotChannel_KeptWhenWithoutSoftMerge(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user lives in Guangzhou", Subject: "user", Predicate: "lives_in"}},
			{{Content: "user lives in Shanghai", Subject: "user", Predicate: "lives_in"}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithoutSoftMerge(), // vector channel off, slot channel still on.
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()
	first, _ := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "我住广州"}}},
	})
	second, _ := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "搬上海"}}},
	})
	doc, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), first.EntryIDs[0])
	if got := doc.Metadata[recall.MetaSupersededBy]; got != second.EntryIDs[0] {
		t.Fatalf("WithoutSoftMerge must NOT disable slot channel; superseded_by=%v, want %q", got, second.EntryIDs[0])
	}
}

// TestSlotChannel_DisabledByWithoutSlotChannel exercises the new
// orthogonal knob: WithoutSlotChannel suppresses the deterministic
// path even though the vector channel is still ON. With no embedder
// configured, the vector path also no-ops, so the older entry must
// be left untagged — proving the slot path was the only thing
// touching it before.
func TestSlotChannel_DisabledByWithoutSlotChannel(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user lives in Guangzhou", Subject: "user", Predicate: "lives_in"}},
			{{Content: "user lives in Shanghai", Subject: "user", Predicate: "lives_in"}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithoutSlotChannel(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()
	first, _ := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "我住广州"}}},
	})
	_, _ = m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "搬上海"}}},
	})
	doc, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), first.EntryIDs[0])
	if v, has := doc.Metadata[recall.MetaSupersededBy]; has {
		t.Fatalf("WithoutSlotChannel must suppress slot tagging; got superseded_by=%v", v)
	}
}

// TestSlotSupersede_MultipleStaleEntriesInOneSlot covers the
// rarely-hit but important case the original review flagged: a slot
// already contains MORE THAN ONE untagged stale entry (e.g. a backfill
// that replayed history out of order, or an external importer that
// bypassed the slot channel). The newest write must tag every
// previously-untagged entry, not just one.
func TestSlotSupersede_MultipleStaleEntriesInOneSlot(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	// Drop AgentID so the seeded raw docs (which have no agent_id
	// metadata) survive AgentRecallFilter — the test exercises the
	// supersede channel, not the agent-isolation filter.
	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}
	ns := recall.NamespaceFor(scope)
	// Pre-seed two untagged entries sharing the same slot_key — this
	// is the kind of state the slot channel itself never produces
	// (it always skips already-tagged entries) but a backfill does.
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{
			ID: "stale-1", Content: "user lives in Guangzhou",
			Metadata: map[string]any{
				recall.MetaSubject:   "user",
				recall.MetaPredicate: "lives_in",
				recall.MetaSlotKey:   "user|lives_in",
			},
			Timestamp: time.Now().Add(-2 * time.Hour),
		},
		{
			ID: "stale-2", Content: "user lives in Shenzhen",
			Metadata: map[string]any{
				recall.MetaSubject:   "user",
				recall.MetaPredicate: "lives_in",
				recall.MetaSlotKey:   "user|lives_in",
			},
			Timestamp: time.Now().Add(-time.Hour),
		},
	})

	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "user lives in Beijing", Subject: "user", Predicate: "lives_in"}},
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
	res, _ := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "搬北京"}}},
	})
	if len(res.EntryIDs) != 1 {
		t.Fatalf("expected one new entry, got %+v", res.EntryIDs)
	}
	newID := res.EntryIDs[0]
	for _, oldID := range []string{"stale-1", "stale-2"} {
		d, _, _ := idx.Get(ctx, ns, oldID)
		if got := d.Metadata[recall.MetaSupersededBy]; got != newID {
			t.Fatalf("%s.superseded_by=%v, want %q (slot channel must tag every untagged stale entry)", oldID, got, newID)
		}
	}
}

// TestSlotEligibility_RejectsDelimiterInSubject verifies the M2 fix:
// a fact whose subject contains the slot delimiter '|' MUST NOT
// produce a slot_key (otherwise it would collide with another
// fact's legitimate slot). The fact still gets written, just
// without slot metadata, and falls back to the resolver / vector
// channels.
func TestSlotEligibility_RejectsDelimiterInSubject(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "ambiguous fact", Subject: "user|alt", Predicate: "lives_in"}},
		},
	}
	m, err := recall.New(idx, recall.WithRequireUserID(), recall.WithExtractor(ex))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()
	res, _ := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "x"}}},
	})
	doc, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), res.EntryIDs[0])
	if v, has := doc.Metadata[recall.MetaSlotKey]; has {
		t.Fatalf("subject with delimiter must NOT produce slot_key; got %v", v)
	}
	if v, has := doc.Metadata[recall.MetaSubject]; has {
		t.Fatalf("subject metadata must be skipped together with slot_key; got %v", v)
	}
}

// TestSlotCollapse_EndToEnd_OnLegacyData wires WithSlotCollapse into
// the LTM pipeline and asserts that even when both old and new
// entries are returned by the recall lane (the older one was never
// tagged with superseded_by — i.e. legacy data), only the newest
// per slot survives. Without WithSlotCollapse the older entry would
// remain visible in the hits, defeating the point of the stage.
//
// This is the read-side counterpart to the slot supersede tests
// above: it lives next to them in merger_test.go because the
// collapse stage is the safety net for whatever the supersede
// channel could not tag (legacy / out-of-band writes).
func TestSlotCollapse_EndToEnd_OnLegacyData(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}
	ns := recall.NamespaceFor(scope)
	now := time.Now()
	// Two entries, same slot, neither tagged with superseded_by — the
	// "untouched legacy data" case WithSlotCollapse exists for.
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{
			ID: "old", Content: "user lives in Guangzhou",
			Metadata:  map[string]any{recall.MetaSlotKey: "user|lives_in"},
			Timestamp: now.Add(-time.Hour),
		},
		{
			ID: "new", Content: "user lives in Shanghai",
			Metadata:  map[string]any{recall.MetaSlotKey: "user|lives_in"},
			Timestamp: now,
		},
	})

	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithPipeline(pipeline.LTM(nil, pipeline.WithSlotCollapse(true))),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	hits, err := m.Recall(ctx, scope, recall.Request{Query: "lives", TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Entry.ID == "old" {
			t.Fatalf("WithSlotCollapse must drop legacy older entry; hits=%+v", hits)
		}
	}
	var sawNew bool
	for _, h := range hits {
		if h.Entry.ID == "new" {
			sawNew = true
		}
	}
	if !sawNew {
		t.Fatalf("expected newest entry to survive collapse, hits=%+v", hits)
	}
}
