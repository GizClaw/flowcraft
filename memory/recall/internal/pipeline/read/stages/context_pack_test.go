package stages

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
)

type reorderReranker struct{}

func (reorderReranker) Rerank(_ context.Context, _ string, hits []domain.Hit) ([]domain.Hit, error) {
	if len(hits) < 2 {
		return hits, nil
	}
	return []domain.Hit{hits[1], hits[0]}, nil
}

type inspectEvidenceReranker struct {
	counts []int
}

func (r *inspectEvidenceReranker) Rerank(_ context.Context, _ string, hits []domain.Hit) ([]domain.Hit, error) {
	r.counts = r.counts[:0]
	for _, hit := range hits {
		r.counts = append(r.counts, len(hit.Evidence))
	}
	return hits, nil
}

type emptyReranker struct{}

func (emptyReranker) Rerank(_ context.Context, _ string, _ []domain.Hit) ([]domain.Hit, error) {
	return nil, nil
}

func TestContextPackSnapshotsInputRerankedAndFinal(t *testing.T) {
	stage := NewContextPack(reorderReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "where did alice go"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "evidence", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "evidence", EvidenceRefs: []domain.EvidenceRef{{ID: "e1"}}},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "selected evidence"}},
			},
			{
				Candidate: domain.Candidate{FactID: "distractor", Source: "entity", Score: 0.8, EvidenceIDs: []string{"e2"}},
				Fact:      domain.TemporalFact{ID: "distractor", EvidenceRefs: []domain.EvidenceRef{{ID: "e2"}}},
				Evidence:  []domain.EvidenceRef{{ID: "e2", Text: "selected distractor"}},
			},
		},
	}
	state.EnsureTrace()

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.ContextPackDetail)
	if got.Input == nil || len(*got.Input) != 2 || (*got.Input)[0].FactID != "evidence" {
		t.Fatalf("input snapshots = %+v", got.Input)
	}
	if got.RerankedHits == nil || len(*got.RerankedHits) != 2 || (*got.RerankedHits)[0].FactID != "distractor" {
		t.Fatalf("reranked snapshots = %+v", got.RerankedHits)
	}
	if got.Hits == nil || len(*got.Hits) != 1 || (*got.Hits)[0].FactID != "evidence" {
		t.Fatalf("final snapshots = %+v", got.Hits)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "evidence" {
		t.Fatalf("state hits = %+v", state.Hits)
	}
	if len(state.Hits[0].Evidence) != 1 || state.Hits[0].Evidence[0].Text != "selected evidence" {
		t.Fatalf("hit evidence should survive context_pack/rerank: %+v", state.Hits[0].Evidence)
	}
}

func TestBuildGroundedHitsDoesNotAffectRerankerInput(t *testing.T) {
	reranker := &inspectEvidenceReranker{}
	stage := NewContextPack(reranker)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "Where did Caroline move from?"},
		Ranked: []domain.ContextItem{{
			Candidate: domain.Candidate{FactID: "move", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
			Fact: domain.TemporalFact{
				ID:      "move",
				Kind:    domain.KindState,
				Content: "Caroline moved from her home country.",
				EvidenceRefs: []domain.EvidenceRef{
					{ID: "e1", Text: "Caroline moved from her home country four years ago."},
					{ID: "e2", Text: "Caroline said Sweden is where she moved from."},
				},
			},
			Evidence: []domain.EvidenceRef{{ID: "e1", Text: "Caroline moved from her home country four years ago."}},
		}},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(reranker.counts) != 1 || reranker.counts[0] != 1 {
		t.Fatalf("reranker should receive only candidate evidence, got counts %+v", reranker.counts)
	}
	if _, err := NewBuildGroundedHits().Run(context.Background(), state); err != nil {
		t.Fatalf("grounding Run returned error: %v", err)
	}
	if ids := evidenceIDs(state.Hits[0].Evidence); len(ids) != 2 || ids[0] != "e1" || ids[1] != "e2" {
		t.Fatalf("final output should still include supporting evidence, got %+v", ids)
	}
}

func TestContextPackSkipsSnapshotsWithoutTrace(t *testing.T) {
	stage := NewContextPack(reorderReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "where did alice go"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "evidence", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "evidence", EvidenceRefs: []domain.EvidenceRef{{ID: "e1"}}},
			},
			{
				Candidate: domain.Candidate{FactID: "distractor", Source: "entity", Score: 0.8, EvidenceIDs: []string{"e2"}},
				Fact:      domain.TemporalFact{ID: "distractor", EvidenceRefs: []domain.EvidenceRef{{ID: "e2"}}},
			},
		},
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := detail.(diagnostic.ContextPackDetail)
	if got.Input != nil || got.RerankedHits != nil || got.Hits != nil {
		t.Fatalf("snapshots should be nil on nil-trace path: %+v", got)
	}
	if got.InputCount != 2 || got.Reranked != 2 || got.Count != 1 {
		t.Fatalf("counts should still be populated: %+v", got)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "evidence" {
		t.Fatalf("state hits = %+v", state.Hits)
	}
}

func TestContextPackKeepsQueryRelevantContext(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "When did Alice buy 2 ceramic figurines?"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "distractor", Source: "entity", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "distractor", Kind: domain.KindState, Content: "Alice likes Paris."},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice likes Paris."}},
			},
		},
		AfterTrust: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "distractor", Source: "entity", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "distractor", Kind: domain.KindState, Content: "Alice likes Paris."},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice likes Paris."}},
			},
			{
				Candidate: domain.Candidate{FactID: "specific", Source: "retrieval", Score: 0.2, EvidenceIDs: []string{"e2"}},
				Fact:      domain.TemporalFact{ID: "specific", Kind: domain.KindEvent, Content: "Alice bought 2 ceramic figurines."},
				Evidence:  []domain.EvidenceRef{{ID: "e2", Text: "On 2023-05-07 Alice bought 2 ceramic figurines."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "specific" {
		t.Fatalf("context packer should keep query-relevant evidence, got %+v", state.Hits)
	}
}

func TestContextPackKeepsBestContextForSharedEvidence(t *testing.T) {
	stage := NewContextPack(nil)
	shared := domain.EvidenceRef{ID: "e1", Text: "I painted that lake sunrise last year!"}
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "When did Melanie paint a sunrise?"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "state", Source: "graph", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "state", Kind: domain.KindState, Content: "Melanie's painting of the lake sunrise is special to her.", EvidenceRefs: []domain.EvidenceRef{shared}},
				Evidence:  []domain.EvidenceRef{shared},
			},
			{
				Candidate: domain.Candidate{FactID: "event", Source: "retrieval", Score: 0.8, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "event", Kind: domain.KindEvent, Content: "Melanie painted a lake sunrise last year.", EvidenceRefs: []domain.EvidenceRef{shared}},
				Evidence:  []domain.EvidenceRef{shared},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "event" {
		t.Fatalf("shared evidence representative should keep the more query-relevant context, got %+v", state.Hits)
	}
}

func TestContextPackCoversQueryAnchors(t *testing.T) {
	stage := NewContextPack(nil)
	query := "What books and instruments does Alice like?"
	weak := []domain.ContextItem{
		weakContextItem("weak-1", "e1", "Bob visited Paris."),
		weakContextItem("weak-2", "e2", "Carol likes hiking."),
		weakContextItem("weak-3", "e3", "Dylan cooked soup."),
		weakContextItem("weak-4", "e4", "Eve bought pottery."),
	}
	strong := []domain.ContextItem{
		strongContextItem("book", "e5", "Alice likes books such as Charlotte's Web."),
		strongContextItem("violin", "e6", "Alice likes instruments such as the violin."),
		strongContextItem("clarinet", "e7", "Alice likes instruments such as the clarinet."),
		strongContextItem("guitar", "e8", "Alice likes instruments such as the guitar."),
	}
	state := &read.ReadState{
		Plan:       &domain.QueryPlan{TotalCap: 4},
		Query:      domain.Query{Text: query},
		Ranked:     append([]domain.ContextItem(nil), weak...),
		AfterTrust: append(append([]domain.ContextItem(nil), weak...), strong...),
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := map[string]bool{}
	for _, hit := range state.Hits {
		got[hit.Fact.ID] = true
	}
	if !got["book"] {
		t.Fatalf("expected context covering the books anchor, got %+v", state.Hits)
	}
	instrumentContexts := 0
	for _, id := range []string{"violin", "clarinet", "guitar"} {
		if got[id] {
			instrumentContexts++
		}
	}
	if instrumentContexts < 2 {
		t.Fatalf("expected diverse context covering instruments, got %+v", state.Hits)
	}
}

func TestContextPackUsesWiderPoolForCoverage(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 2},
		Query: domain.Query{Text: "When did Alice buy 2 ceramic figurines?"},
		Ranked: []domain.ContextItem{
			weakContextItem("weak-1", "e1", "Bob visited Paris."),
			weakContextItem("weak-2", "e2", "Carol likes hiking."),
		},
		AfterTrust: []domain.ContextItem{
			weakContextItem("weak-1", "e1", "Bob visited Paris."),
			weakContextItem("weak-2", "e2", "Carol likes hiking."),
			{
				Candidate: domain.Candidate{FactID: "specific", Source: "retrieval", Score: 0.15, EvidenceIDs: []string{"e3"}},
				Fact:      domain.TemporalFact{ID: "specific", Kind: domain.KindEvent, Content: "Alice bought 2 ceramic figurines."},
				Evidence:  []domain.EvidenceRef{{ID: "e3", Text: "On 2023-05-07 Alice bought 2 ceramic figurines."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := map[string]bool{}
	for _, hit := range state.Hits {
		got[hit.Fact.ID] = true
	}
	if !got["specific"] {
		t.Fatalf("context packer should keep relevant evidence from the wider pool, got %+v", state.Hits)
	}
}

func TestContextPackUsesFactContentWhenEvidenceIsThin(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "What instrument does Alice play?"},
		Ranked: []domain.ContextItem{
			weakContextItem("weak", "e1", "Bob visited Paris."),
		},
		AfterTrust: []domain.ContextItem{
			weakContextItem("weak", "e1", "Bob visited Paris."),
			{
				Candidate: domain.Candidate{FactID: "instrument", Source: "graph", Score: 0.1, EvidenceIDs: []string{"e2"}},
				Fact: domain.TemporalFact{
					ID:        "instrument",
					Kind:      domain.KindPreference,
					Content:   "Alice plays the violin.",
					Subject:   "Alice",
					Predicate: "plays",
					Object:    "violin",
				},
				Evidence: []domain.EvidenceRef{{ID: "e2", Text: "She mentioned it."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "instrument" {
		t.Fatalf("context packer should score fact content as well as evidence text, got %+v", state.Hits)
	}
}

func TestContextPackKeepsCollectionSiblings(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 2},
		Query: domain.Query{Text: "What items has Alice bought?"},
		Ranked: []domain.ContextItem{
			contextItemWithSource("figurines", "e1", "retrieval", 0.90, "Alice bought ceramic figurines."),
			contextItemWithSource("paris", "e2", "entity", 0.88, "Alice likes Paris."),
		},
		AfterTrust: []domain.ContextItem{
			contextItemWithSource("figurines", "e1", "retrieval", 0.90, "Alice bought ceramic figurines."),
			contextItemWithSource("paris", "e2", "entity", 0.88, "Alice likes Paris."),
			contextItemWithSource("shoes", "e3", "retrieval", 0.20, "Alice bought red shoes."),
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := map[string]bool{}
	for _, hit := range state.Hits {
		got[hit.Fact.ID] = true
	}
	if !got["figurines"] || !got["shoes"] {
		t.Fatalf("collection packing should keep sibling purchased items, got %+v", state.Hits)
	}
}

func TestContextPackPreservesLowScoreSetSiblings(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 3},
		Query: domain.Query{Text: "What pets does Melanie have?"},
		Ranked: []domain.ContextItem{
			contextItemWithStructuredFact("bailey", "e1", "retrieval", 0.90, "Melanie has a cat named Bailey.", "Melanie", "has_pet", "Bailey"),
			contextItemWithStructuredFact("hiking", "e2", "graph", 0.88, "Melanie went hiking with her family.", "Melanie", "went", "hiking"),
			contextItemWithStructuredFact("pottery", "e3", "entity", 0.86, "Melanie enjoys pottery class.", "Melanie", "enjoys", "pottery"),
		},
		AfterTrust: []domain.ContextItem{
			contextItemWithStructuredFact("bailey", "e1", "retrieval", 0.90, "Melanie has a cat named Bailey.", "Melanie", "has_pet", "Bailey"),
			contextItemWithStructuredFact("hiking", "e2", "graph", 0.88, "Melanie went hiking with her family.", "Melanie", "went", "hiking"),
			contextItemWithStructuredFact("pottery", "e3", "entity", 0.86, "Melanie enjoys pottery class.", "Melanie", "enjoys", "pottery"),
			contextItemWithStructuredFact("oliver", "e4", "retrieval", 0.12, "Melanie has a pet dog named Oliver.", "Melanie", "has_pet", "Oliver"),
			contextItemWithStructuredFact("luna", "e5", "retrieval", 0.10, "Melanie has a pet dog named Luna.", "Melanie", "has_pet", "Luna"),
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := map[string]bool{}
	for _, hit := range state.Hits {
		got[hit.Fact.ID] = true
	}
	for _, id := range []string{"bailey", "oliver", "luna"} {
		if !got[id] {
			t.Fatalf("set-completion packing should preserve low-score sibling %q, got %+v", id, state.Hits)
		}
	}
}

func TestContextPackKeepsBridgeAssociatedEvidence(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 2},
		Query: domain.Query{Text: "Where did Alice buy the necklace that she wore?"},
		Ranked: []domain.ContextItem{
			contextItemWithSource("wore-necklace", "D1:1", "retrieval", 0.90, "Alice wore the necklace to dinner."),
			contextItemWithSource("dog", "D2:1", "entity", 0.88, "Alice walked her dog."),
		},
		AfterTrust: []domain.ContextItem{
			contextItemWithSource("wore-necklace", "D1:1", "retrieval", 0.90, "Alice wore the necklace to dinner."),
			contextItemWithSource("dog", "D2:1", "entity", 0.88, "Alice walked her dog."),
			contextItemWithSource("bought-necklace", "D1:2", "retrieval", 0.20, "Alice bought the necklace in Paris."),
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := map[string]bool{}
	for _, hit := range state.Hits {
		got[hit.Fact.ID] = true
	}
	if !got["wore-necklace"] || !got["bought-necklace"] {
		t.Fatalf("bridge packing should keep associated evidence from the same group, got %+v", state.Hits)
	}
}

func TestContextPackerSignalCoverageReplacesWeakDuplicateContext(t *testing.T) {
	features := domain.QueryFeatures{
		Tokens: map[string]struct{}{
			"alice":       {},
			"books":       {},
			"instruments": {},
		},
	}
	queryFeatures := newContextPackQueryFeatures("", features)
	selectedCandidates := []contextPackCandidate{
		coverageCandidate("generic-1", 6, 0.20, map[string]struct{}{"alice": {}}),
		coverageCandidate("generic-2", 7, 0.18, map[string]struct{}{"alice": {}}),
	}
	selected := contextPackHits(selectedCandidates)
	rescue := coverageCandidate("rescue", 15, 0.55, map[string]struct{}{
		"alice":       {},
		"books":       {},
		"instruments": {},
		"violin":      {},
	})

	got, gotCandidates := contextPackEnsureSignalCoverage(queryFeatures, append(selectedCandidates, rescue), selected, selectedCandidates)
	if len(got) != 2 || len(gotCandidates) != 2 {
		t.Fatalf("context coverage lengths = hits:%d candidates:%d", len(got), len(gotCandidates))
	}
	found := false
	for _, hit := range got {
		if hit.Fact.ID == "rescue" {
			found = true
		}
	}
	if !found {
		t.Fatalf("context packer should add the candidate covering missing query anchors, got %+v", got)
	}
}

func TestContextPackRerankerPathUsesContextPacker(t *testing.T) {
	stage := NewContextPack(reorderReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "What instrument does Alice play?"},
		Ranked: []domain.ContextItem{
			weakContextItem("weak", "e1", "Bob visited Paris."),
		},
		AfterTrust: []domain.ContextItem{
			weakContextItem("weak", "e1", "Bob visited Paris."),
			{
				Candidate: domain.Candidate{FactID: "instrument", Source: "graph", Score: 0.1, EvidenceIDs: []string{"e2"}},
				Fact: domain.TemporalFact{
					ID:        "instrument",
					Kind:      domain.KindPreference,
					Content:   "Alice plays the violin.",
					Subject:   "Alice",
					Predicate: "plays",
					Object:    "violin",
				},
				Evidence: []domain.EvidenceRef{{ID: "e2", Text: "She mentioned it."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "instrument" {
		t.Fatalf("reranker path should use context packer, got %+v", state.Hits)
	}
}

func TestContextPackFallsBackToPoolWhenRerankerReturnsEmpty(t *testing.T) {
	stage := NewContextPack(emptyReranker{})
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "What instrument does Alice play?"},
		Ranked: []domain.ContextItem{
			weakContextItem("weak", "e1", "Bob visited Paris."),
		},
		AfterTrust: []domain.ContextItem{
			weakContextItem("weak", "e1", "Bob visited Paris."),
			{
				Candidate: domain.Candidate{FactID: "instrument", Source: "graph", Score: 0.1, EvidenceIDs: []string{"e2"}},
				Fact: domain.TemporalFact{
					ID:        "instrument",
					Kind:      domain.KindPreference,
					Content:   "Alice plays the violin.",
					Subject:   "Alice",
					Predicate: "plays",
					Object:    "violin",
				},
				Evidence: []domain.EvidenceRef{{ID: "e2", Text: "She mentioned it."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 1 || state.Hits[0].Fact.ID != "instrument" {
		t.Fatalf("context packer should fall back to the wider pool when reranker returns empty, got %+v", state.Hits)
	}
}

func TestContextPackDedupesSameEvidence(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 2},
		Query: domain.Query{Text: "What did Alice buy?"},
		Ranked: []domain.ContextItem{
			{
				Candidate: domain.Candidate{FactID: "a", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "a", Kind: domain.KindEvent, Content: "Alice bought pottery."},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice bought pottery."}},
			},
			{
				Candidate: domain.Candidate{FactID: "b", Source: "graph", Score: 0.8, EvidenceIDs: []string{"e1"}},
				Fact:      domain.TemporalFact{ID: "b", Kind: domain.KindEvent, Content: "Alice purchased pottery."},
				Evidence:  []domain.EvidenceRef{{ID: "e1", Text: "Alice bought pottery."}},
			},
			{
				Candidate: domain.Candidate{FactID: "c", Source: "retrieval", Score: 0.7, EvidenceIDs: []string{"e2"}},
				Fact:      domain.TemporalFact{ID: "c", Kind: domain.KindState, Content: "Alice likes Paris."},
				Evidence:  []domain.EvidenceRef{{ID: "e2", Text: "Alice likes Paris."}},
			},
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(state.Hits) != 2 {
		t.Fatalf("want two final hits after dedupe/fill, got %+v", state.Hits)
	}
	if state.Hits[0].Fact.ID != "a" || state.Hits[1].Fact.ID != "c" {
		t.Fatalf("same evidence id should be deduped while preserving cap, got %+v", state.Hits)
	}
}

func TestContextPackKeepsSourceDiversity(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 3},
		Query: domain.Query{Text: "What did Alice say about pottery class?"},
		Ranked: []domain.ContextItem{
			contextItemWithSource("retrieval-1", "e1", "retrieval", 0.90, "Alice discussed pottery class logistics."),
			contextItemWithSource("retrieval-2", "e2", "retrieval", 0.88, "Alice discussed pottery class timing."),
			contextItemWithSource("retrieval-3", "e3", "retrieval", 0.86, "Alice discussed pottery class supplies."),
		},
		AfterTrust: []domain.ContextItem{
			contextItemWithSource("retrieval-1", "e1", "retrieval", 0.90, "Alice discussed pottery class logistics."),
			contextItemWithSource("retrieval-2", "e2", "retrieval", 0.88, "Alice discussed pottery class timing."),
			contextItemWithSource("retrieval-3", "e3", "retrieval", 0.86, "Alice discussed pottery class supplies."),
			contextItemWithSource("graph-1", "e4", "graph", 0.84, "Alice discussed pottery class project details."),
		},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	gotGraph := false
	for _, hit := range state.Hits {
		for _, source := range hit.Sources {
			if source == "graph" {
				gotGraph = true
			}
		}
	}
	if !gotGraph {
		t.Fatalf("context packer should keep a similarly relevant alternate source, got %+v", state.Hits)
	}
}

func TestBuildGroundedHitsGroundsSelectedEvidenceWithRelevantFactRefs(t *testing.T) {
	stage := NewContextPack(nil)
	state := &read.ReadState{
		Plan:  &domain.QueryPlan{TotalCap: 1},
		Query: domain.Query{Text: "Where did Caroline move from?"},
		Ranked: []domain.ContextItem{{
			Candidate: domain.Candidate{FactID: "move", Source: "retrieval", Score: 0.9, EvidenceIDs: []string{"e1"}},
			Fact: domain.TemporalFact{
				ID:      "move",
				Kind:    domain.KindState,
				Content: "Caroline moved from her home country.",
				EvidenceRefs: []domain.EvidenceRef{
					{ID: "e1", Text: "Caroline moved from her home country four years ago."},
					{ID: "e2", Text: "Caroline said Sweden is where she moved from."},
					{ID: "e3", Text: "Melanie likes pottery classes."},
				},
			},
			Evidence: []domain.EvidenceRef{{ID: "e1", Text: "Caroline moved from her home country four years ago."}},
		}},
	}

	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if _, err := NewBuildGroundedHits().Run(context.Background(), state); err != nil {
		t.Fatalf("grounding Run returned error: %v", err)
	}
	if len(state.Hits) != 1 {
		t.Fatalf("hits = %+v", state.Hits)
	}
	got := evidenceIDs(state.Hits[0].Evidence)
	want := []string{"e1", "e2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("grounding evidence ids = %+v, want %+v", got, want)
	}
}

func TestBuildGroundedHitsEvidenceIsCapped(t *testing.T) {
	refs := []domain.EvidenceRef{
		{ID: "e1", Text: "Alice bought pottery."},
		{ID: "e2", Text: "Alice bought ceramic figurines."},
		{ID: "e3", Text: "Alice bought a violin."},
		{ID: "e4", Text: "Alice bought a book."},
	}
	got := selectGroundingEvidence("What did Alice buy?", []domain.EvidenceRef{refs[0]}, refs)
	if len(got) != maxHitEvidenceRefs {
		t.Fatalf("grounding evidence count = %d, want %d: %+v", len(got), maxHitEvidenceRefs, got)
	}
	if got[0].ID != "e1" {
		t.Fatalf("selected evidence should stay first, got %+v", evidenceIDs(got))
	}
}

func TestBuildGroundedHitsSkipsWeakStopwordOrEntityOnlyRefs(t *testing.T) {
	refs := []domain.EvidenceRef{
		{ID: "e1", Text: "Melanie has two cats named Oscar and Luna."},
		{ID: "e2", Text: "Melanie went hiking with her family."},
		{ID: "e3", Text: "The pottery class was relaxing."},
	}
	got := selectGroundingEvidence("What pets does Melanie have?", []domain.EvidenceRef{refs[0]}, refs)
	if ids := evidenceIDs(got); len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("weak entity-only refs should not be added, got %+v", ids)
	}
}

func TestBuildGroundedHitsKeepsTimestampedTemporalSupport(t *testing.T) {
	refs := []domain.EvidenceRef{
		{ID: "e1", Text: "Melanie painted a lake sunrise last year."},
		{ID: "e2", Text: "Melanie shared the sunrise painting with Caroline.", Timestamp: time.Date(2023, 5, 8, 13, 56, 0, 0, time.UTC)},
		{ID: "e3", Text: "Caroline discussed adoption paperwork."},
	}
	got := selectGroundingEvidence("When did Melanie paint a sunrise?", []domain.EvidenceRef{refs[0]}, refs)
	ids := evidenceIDs(got)
	if len(ids) != 2 || ids[0] != "e1" || ids[1] != "e2" {
		t.Fatalf("timestamped temporal support should be added, got %+v", ids)
	}
}

func BenchmarkPackRecallContext(b *testing.B) {
	query := "When did Alice buy 2 ceramic figurines and which instrument does she play?"
	ordered := make([]domain.Hit, 0, 30)
	pool := make([]domain.Hit, 0, 120)
	for i := 0; i < 120; i++ {
		id := "hit-" + strconv.Itoa(i)
		evidenceID := "e-" + strconv.Itoa(i)
		marker := "marker" + strconv.Itoa(i)
		text := "Bob visited Paris and Carol likes hiking near " + marker + "."
		content := text
		score := 1.0 - float64(i)*0.005
		if i%17 == 0 {
			text = "On 2023-05-07 Alice bought 2 ceramic figurines and later played the violin."
			content = "Alice bought 2 ceramic figurines and plays the violin."
			score = 0.15
		}
		hit := domain.Hit{
			Fact: domain.TemporalFact{
				ID:      id,
				Kind:    domain.KindEvent,
				Content: content,
				Subject: "Alice",
			},
			Evidence: []domain.EvidenceRef{{ID: evidenceID, Text: text}},
			Score:    score,
			Sources:  []string{"retrieval"},
		}
		pool = append(pool, hit)
		if i < 30 {
			ordered = append(ordered, hit)
		}
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		hits := packRecallContext(query, ordered, pool, 30)
		if len(hits) != 30 {
			b.Fatalf("len(hits) = %d, want 30", len(hits))
		}
	}
}

func evidenceIDs(refs []domain.EvidenceRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.ID)
	}
	return out
}

func weakContextItem(id, evidenceID, text string) domain.ContextItem {
	return domain.ContextItem{
		Candidate: domain.Candidate{FactID: id, Source: "retrieval", Score: 0.9, EvidenceIDs: []string{evidenceID}},
		Fact:      domain.TemporalFact{ID: id, Kind: domain.KindState, Content: text},
		Evidence:  []domain.EvidenceRef{{ID: evidenceID, Text: text}},
	}
}

func strongContextItem(id, evidenceID, text string) domain.ContextItem {
	return domain.ContextItem{
		Candidate: domain.Candidate{FactID: id, Source: "retrieval", Score: 0.2, EvidenceIDs: []string{evidenceID}},
		Fact:      domain.TemporalFact{ID: id, Kind: domain.KindState, Content: text},
		Evidence:  []domain.EvidenceRef{{ID: evidenceID, Text: text}},
	}
}

func contextItemWithSource(id, evidenceID, source string, score float64, text string) domain.ContextItem {
	return domain.ContextItem{
		Candidate: domain.Candidate{FactID: id, Source: source, Score: score, EvidenceIDs: []string{evidenceID}},
		Fact:      domain.TemporalFact{ID: id, Kind: domain.KindState, Content: text},
		Evidence:  []domain.EvidenceRef{{ID: evidenceID, Text: text}},
	}
}

func contextItemWithStructuredFact(id, evidenceID, source string, score float64, text, subject, predicate, object string) domain.ContextItem {
	return domain.ContextItem{
		Candidate: domain.Candidate{FactID: id, Source: source, Score: score, EvidenceIDs: []string{evidenceID}},
		Fact: domain.TemporalFact{
			ID:        id,
			Kind:      domain.KindState,
			Content:   text,
			Subject:   subject,
			Predicate: predicate,
			Object:    object,
		},
		Evidence: []domain.EvidenceRef{{ID: evidenceID, Text: text}},
	}
}

func coverageCandidate(id string, rank int, score float64, tokens map[string]struct{}) contextPackCandidate {
	return contextPackCandidate{
		hit: domain.Hit{
			Fact: domain.TemporalFact{ID: id, Kind: domain.KindState, Content: id},
		},
		score:          score,
		baseScore:      score,
		textScore:      score,
		queryRank:      rank,
		evidenceTokens: tokens,
		factTokens:     tokens,
	}
}
