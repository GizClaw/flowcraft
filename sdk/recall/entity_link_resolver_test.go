package recall_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func msgsFromText(s string) []llm.Message {
	return []llm.Message{{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: s}}}}
}

// TestScopeFromNamespace_RoundTrip pins the (Scope → namespace →
// Scope) round-trip for the shapes [NamespaceFor] is allowed to
// emit. RuntimeID / UserID arrive sane'd, so we feed pre-sanitised
// inputs to make the symmetry explicit.
func TestScopeFromNamespace_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   recall.Scope
	}{
		{"user_scope", recall.Scope{RuntimeID: "rt1", UserID: "u1"}},
		{"global_scope", recall.Scope{RuntimeID: "rt1"}},
		{"empty_runtime_falls_back_to_anon", recall.Scope{UserID: "u1"}},
		{"alphanumeric_user", recall.Scope{RuntimeID: "rt1", UserID: "conv_26"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := recall.NamespaceFor(tc.in)
			got, ok := recall.ScopeFromNamespace(ns)
			if !ok {
				t.Fatalf("ScopeFromNamespace(%q) failed", ns)
			}
			// RuntimeID may be sane'd ("" -> "anon").
			wantRT := tc.in.RuntimeID
			if wantRT == "" {
				wantRT = "anon"
			}
			if got.RuntimeID != wantRT {
				t.Fatalf("RuntimeID: got %q; want %q (ns=%q)", got.RuntimeID, wantRT, ns)
			}
			if got.UserID != tc.in.UserID {
				t.Fatalf("UserID: got %q; want %q (ns=%q)", got.UserID, tc.in.UserID, ns)
			}
		})
	}
}

func TestScopeFromNamespace_RejectsUnknownGrammar(t *testing.T) {
	for _, ns := range []string{
		"",
		"random-namespace",
		"ltm_only_prefix",
		"prefix_ltm_rt__global",
	} {
		if _, ok := recall.ScopeFromNamespace(ns); ok {
			t.Fatalf("ScopeFromNamespace(%q) should fail", ns)
		}
	}
}

// TestInternalEntityLinkResolver_BridgesEntityStoreLookup
// exercises Save → EntityStore → resolver → CandidateEntityIDs by
// running through the public Memory.Recall path with multi-recall
// and the entity-link lane wired in by [recall.WithEntityStore].
//
// The lane materialises via DocGetter, so missed ids (e.g. typos)
// surface as zero candidates; hits that survive the lane appear in
// Recall output.
func TestInternalEntityLinkResolver_BridgesEntityStoreLookup(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[{"content":"Alice loves espresso","entities":["Alice","espresso"]}]`
	m, err := recall.New(idx,
		recall.WithLLM(&stubLLM{resp: resp}),
		recall.WithEntityStore(0),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	scope := recall.Scope{RuntimeID: "rt1", UserID: "u1"}

	// Verify the bridge agrees with the namespace it will be
	// queried under.
	if _, ok := recall.ScopeFromNamespace(recall.NamespaceFor(scope)); !ok {
		t.Fatal("scope round-trip broken")
	}

	if _, err := m.Save(ctx, scope, msgsFromText("I love Alice's espresso")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Direct EntityStore probe: the bridge is unit-tested through
	// the same Lookup the resolver invokes.
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if store == nil {
		t.Skip("memidx does not satisfy DocGetter on this build")
	}
	ids, err := store.Lookup(ctx, scope, []string{"alice"}, 0)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("EntityStore has no link for 'alice'")
	}
}

// fakeEmbedder is a deterministic 3-D embedder whose vector only
// depends on whether the input mentions specific keywords. It lets
// the end-to-end test force the vector lane to MISS the entry while
// the entity-link lane still HITs it — proving the lane is doing
// real work rather than being shadowed by vector recall.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, s string) ([]float32, error) {
	v := []float32{0, 0, 0}
	low := strings.ToLower(s)
	if strings.Contains(low, "alpha") {
		v[0] = 1
	}
	if strings.Contains(low, "beta") {
		v[1] = 1
	}
	if strings.Contains(low, "gamma") {
		v[2] = 1
	}
	return v, nil
}

func (e fakeEmbedder) EmbedBatch(ctx context.Context, in []string) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i, s := range in {
		v, _ := e.Embed(ctx, s)
		out[i] = v
	}
	return out, nil
}

// TestEntityLinkLane_EndToEnd_BridgesSaveToRecall is the
// load-bearing correctness test for the entire entity-store pipeline.
//
// Scenario:
//   - We Save an entry whose CONTENT mentions only "alpha" but whose
//     ENTITIES include "Alice". With our fake embedder the entry's
//     vector is [1, 0, 0].
//   - We Recall with a query that mentions only "beta gamma Alice".
//     The query vector is [0, 1, 1], so cosine with the entry is 0
//     — vector lane MISSES. BM25 also misses (no token overlap).
//     The only way the entry can surface in the final hits is via
//     the entity-link lane (Alice → Save's entry id → DocGetter →
//     Hit).
//   - We pick a UserID with a hyphen ("conv-26") so the saneNS
//     round-trip through ScopeFromNamespace is exercised end-to-end.
//
// What this protects:
//   - EntityKey encoding agrees with EntityStore.Lookup encoding (the
//     §14-row-6 issue the user surfaced).
//   - WithEntityStore auto-wires multi-recall + lane + resolver.
//   - The bridge correctly threads namespace → Scope → Lookup.
//   - ModeEntityLink lane materialises hits via DocGetter and feeds
//     RRF.
//   - The hit survives to Memory.Recall's final output.
func TestEntityLinkLane_EndToEnd_BridgesSaveToRecall(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	// Extractor returns a fact whose content is intentionally
	// disjoint from any reasonable query phrasing.
	resp := `[{"content":"alpha protocol notes","entities":["Alice"]}]`
	m, err := recall.New(idx,
		recall.WithLLM(&stubLLM{resp: resp}),
		recall.WithEmbedder(fakeEmbedder{}),
		recall.WithEntityStore(0),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "conv-26"}
	saved, err := m.Save(ctx, scope, msgsFromText("alpha"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(saved.EntryIDs) != 1 {
		t.Fatalf("expected 1 saved entry; got %v", saved.EntryIDs)
	}
	wantID := saved.EntryIDs[0]

	// Sanity: probe the raw EntityStore Lookup using the same
	// hyphenated UserID that triggers saneNS. Without the
	// saneNS-symmetric EntityKey this Lookup would miss.
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if store == nil {
		t.Skip("memidx does not satisfy DocGetter")
	}
	storeHit, _ := store.Lookup(ctx, scope, []string{"Alice"}, 0)
	if len(storeHit) == 0 || storeHit[0] != wantID {
		t.Fatalf("EntityStore.Lookup(hyphenated user) returned %v; want [%s] — saneNS asymmetry?", storeHit, wantID)
	}

	// Recall with a query whose vector ([0,1,1]) is orthogonal to
	// the entry's vector ([1,0,0]) and whose tokens share no BM25
	// overlap with the entry content. The only way to reach the
	// entry is via Alice → entity_link lane.
	rx, ok := m.(recall.RecallExplainer)
	if !ok {
		t.Fatal("Memory must implement RecallExplainer")
	}
	hits, exec, err := rx.RecallExplain(ctx, scope, recall.Request{
		Query: "beta gamma Alice",
		TopK:  5,
		Debug: retrieval.SearchDebug{IncludeLanes: true},
	})
	if err != nil {
		t.Fatalf("RecallExplain: %v", err)
	}
	if exec == nil {
		t.Fatal("expected non-nil Execution with IncludeLanes=true")
	}

	// 1. Final hits must include the entry — proving the lane
	// reached the user-visible output.
	found := false
	for _, h := range hits {
		if h.Entry.ID == wantID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("entry %s did not appear in final hits; got %d hits — entity_link lane is not reaching Recall output", wantID, len(hits))
	}

	// 2. Among lanes, entity_link must specifically be the source.
	var entityLinkHits int
	var vectorHits, bm25Hits int
	for _, lane := range exec.Lanes {
		switch lane.Key {
		case retrieval.LaneEntityLink:
			for _, h := range lane.Hits {
				if h.Doc.ID == wantID {
					entityLinkHits++
				}
			}
		case retrieval.LaneVector:
			vectorHits = len(lane.Hits)
		case retrieval.LaneBM25:
			bm25Hits = len(lane.Hits)
		}
	}
	if entityLinkHits == 0 {
		t.Fatalf("entity_link lane did NOT contribute the entry; exec.Lanes=%+v", exec.Lanes)
	}
	// Vector + BM25 should miss (orthogonal vector, no token
	// overlap). If they were producing hits the test wouldn't
	// isolate the entity-link channel. We don't fail on these
	// because the fake embedder is a smoke detector, not a strict
	// contract — log them for visibility.
	t.Logf("entity_link contributed: %d hits | vector lane: %d | bm25 lane: %d", entityLinkHits, vectorHits, bm25Hits)
}

// TestEntityLinkLane_RecencyOrderAcrossSaves verifies that when the
// same entity is mentioned across multiple Save calls, the
// EntityStore preserves insertion order (FIFO append) and the
// resolver returns that order to the pipeline so RRF's rank vote
// (1/rank) tracks recency — recent ids beat older ids when both
// would otherwise tie.
//
// Concretely:
//   - Save t1: entity "alice" → entry e1
//   - Save t2: entity "alice" → entry e2
//   - Lookup("alice", cap=0) must return [e1, e2] (FIFO).
//   - Lookup("alice", cap=1) must return [e2] (recency-first cap).
//
// This is the contract the read path relies on: ModeEntityLink
// scores `1.0 / rank`, so a stable, recency-aware order from the
// resolver is what makes "recent linked entry beats old linked
// entry" actually hold in fused output.
func TestEntityLinkLane_RecencyOrderAcrossSaves(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	first := `[{"content":"alice fact one","entities":["Alice"]}]`
	second := `[{"content":"alice fact two","entities":["Alice"]}]`
	llmRoute := &scriptedLLM{responses: []string{first, second}}
	m, err := recall.New(idx,
		recall.WithLLM(llmRoute),
		recall.WithEmbedder(fakeEmbedder{}),
		recall.WithEntityStore(0),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "conv-30"}
	r1, err := m.Save(ctx, scope, msgsFromText("turn 1"))
	if err != nil || len(r1.EntryIDs) != 1 {
		t.Fatalf("Save t1: err=%v ids=%v", err, r1.EntryIDs)
	}
	r2, err := m.Save(ctx, scope, msgsFromText("turn 2"))
	if err != nil || len(r2.EntryIDs) != 1 {
		t.Fatalf("Save t2: err=%v ids=%v", err, r2.EntryIDs)
	}
	e1, e2 := r1.EntryIDs[0], r2.EntryIDs[0]
	if e1 == e2 {
		t.Fatalf("expected distinct entry ids across turns; both = %s", e1)
	}

	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if store == nil {
		t.Skip("memidx does not satisfy DocGetter")
	}

	// FIFO order: oldest first.
	full, _ := store.Lookup(ctx, scope, []string{"Alice"}, 0)
	if !equalSlice(full, []string{e1, e2}) {
		t.Fatalf("Lookup cap=0 got %v; want [%s %s] (FIFO append)", full, e1, e2)
	}

	// Recency cap=1 must return the LATEST id, matching
	// IndexEntityStore.Lookup's "tail of the list" cap semantics.
	tail, _ := store.Lookup(ctx, scope, []string{"Alice"}, 1)
	if !equalSlice(tail, []string{e2}) {
		t.Fatalf("Lookup cap=1 got %v; want [%s] (recency-first cap)", tail, e2)
	}
}

// TestEntityLinkLane_ForgetRemovesEntryFromLaneOutput is the end-to-
// end counterpart to TestForgetPrunesAllReferences: it asserts that
// after Memory.Forget the entity-link LANE (not just the underlying
// store) stops surfacing the deleted entry in Recall output.
//
// We reuse the orthogonal-vector trick: only the entity_link lane
// can reach the entry, so dropping it from the lane is observable
// as "Recall returns nothing for this query".
func TestEntityLinkLane_ForgetRemovesEntryFromLaneOutput(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[{"content":"alpha-only content","entities":["Bob"]}]`
	m, err := recall.New(idx,
		recall.WithLLM(&stubLLM{resp: resp}),
		recall.WithEmbedder(fakeEmbedder{}),
		recall.WithEntityStore(0),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := recall.Scope{RuntimeID: "rt1", UserID: "conv-99"}
	r, err := m.Save(ctx, scope, msgsFromText("alpha"))
	if err != nil || len(r.EntryIDs) != 1 {
		t.Fatalf("Save: %v ids=%v", err, r.EntryIDs)
	}
	entryID := r.EntryIDs[0]

	// Pre-Forget: Recall finds it via entity_link.
	pre, err := m.Recall(ctx, scope, recall.Request{Query: "beta Bob", TopK: 5})
	if err != nil {
		t.Fatalf("Recall pre: %v", err)
	}
	if !containsHit(pre, entryID) {
		t.Fatalf("pre-Forget: entity_link lane should surface %s; got %d hits", entryID, len(pre))
	}

	// Forget the entry.
	if err := m.Forget(ctx, scope, entryID, "test"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	// Post-Forget: entity_link lane MUST NOT return it. The entry
	// row is gone from the entry namespace (DocGetter returns
	// found=false → lane skips), AND the entity row's linked_ids
	// list no longer references it.
	post, err := m.Recall(ctx, scope, recall.Request{Query: "beta Bob", TopK: 5})
	if err != nil {
		t.Fatalf("Recall post: %v", err)
	}
	if containsHit(post, entryID) {
		t.Fatalf("post-Forget: lane STILL returned %s; got %d hits", entryID, len(post))
	}

	// Double check at the store level: Forget must have rewritten
	// the Bob row to drop entryID from linked_ids. A future Save
	// for a different entity-id pair shouldn't accidentally restore
	// it via cache.
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if store != nil {
		got, _ := store.Lookup(ctx, scope, []string{"Bob"}, 0)
		for _, id := range got {
			if id == entryID {
				t.Fatalf("EntityStore still has dangling pointer to %s", entryID)
			}
		}
	}
}

// scriptedLLM yields a different canned response per Generate call,
// in declaration order. Used by tests that need to script several
// Save turns with distinct extracted facts.
type scriptedLLM struct {
	responses []string
	idx       int
}

func (s *scriptedLLM) Generate(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	resp := ""
	if s.idx < len(s.responses) {
		resp = s.responses[s.idx]
		s.idx++
	}
	return llm.Message{
		Role:  model.RoleAssistant,
		Parts: []model.Part{{Type: model.PartText, Text: resp}},
	}, llm.TokenUsage{}, nil
}

func (s *scriptedLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

func containsHit(hits []recall.Hit, id string) bool {
	for _, h := range hits {
		if h.Entry.ID == id {
			return true
		}
	}
	return false
}
