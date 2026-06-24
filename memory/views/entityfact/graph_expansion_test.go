package entityfact

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/views"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestExpandGraphSourcesNoSeedReturnsNoCandidates(t *testing.T) {
	ctx := context.Background()
	store, scope := newGraphExpansionTestStore(t)
	putGraphTestEntity(t, ctx, store, scope, Entity{ID: "ent_ada", Type: EntityPerson, Name: "Ada"})
	putGraphTestFact(t, ctx, store, scope, Fact{
		ID:              "fact_tea",
		SubjectEntityID: "ent_ada",
		RelationType:    RelationPreference,
		FactText:        "Ada likes tea.",
		SourceRefs:      []views.SourceRef{graphTestRef("conv", "dia-1")},
	})

	result, err := ExpandGraphSources(ctx, store, scope, nil, GraphExpansionOptions{MaxCandidates: 4})
	if err != nil {
		t.Fatalf("ExpandGraphSources error = %v", err)
	}
	if len(result.Seeds) != 0 || len(result.Candidates) != 0 {
		t.Fatalf("result = %+v, want no seeds/candidates", result)
	}
}

func TestExpandGraphSourcesDedupesAndBudgetsCandidates(t *testing.T) {
	ctx := context.Background()
	store, scope := newGraphExpansionTestStore(t)
	putGraphTestEntity(t, ctx, store, scope, Entity{ID: "ent_ada", Type: EntityPerson, Name: "Ada"})
	putGraphTestEntity(t, ctx, store, scope, Entity{ID: "ent_tea", Type: EntityObject, Name: "tea"})
	putGraphTestEntity(t, ctx, store, scope, Entity{ID: "ent_coffee", Type: EntityObject, Name: "coffee"})
	teaFact := putGraphTestFact(t, ctx, store, scope, Fact{
		ID:              "fact_tea",
		SubjectEntityID: "ent_ada",
		ObjectEntityIDs: []EntityID{"ent_tea"},
		RelationType:    RelationPreference,
		FactText:        "Ada likes tea.",
		SourceRefs:      []views.SourceRef{graphTestRef("conv", "dia-1")},
	})
	teaAgainFact := putGraphTestFact(t, ctx, store, scope, Fact{
		ID:              "fact_tea_again",
		SubjectEntityID: "ent_ada",
		ObjectEntityIDs: []EntityID{"ent_tea"},
		RelationType:    RelationPreference,
		FactText:        "Ada drinks tea every morning.",
		SourceRefs:      []views.SourceRef{graphTestRef("conv", "dia-1")},
	})
	putGraphTestFact(t, ctx, store, scope, Fact{
		ID:              "fact_coffee",
		SubjectEntityID: "ent_ada",
		ObjectEntityIDs: []EntityID{"ent_coffee"},
		RelationType:    RelationPreference,
		FactText:        "Ada also likes coffee.",
		SourceRefs:      []views.SourceRef{graphTestRef("conv", "dia-2")},
	})

	result, err := ExpandGraphSources(ctx, store, scope, []GraphSeedFact{
		{Fact: teaFact, Score: 1},
		{Fact: teaAgainFact, Score: 0.9},
	}, GraphExpansionOptions{
		MaxCandidates:             1,
		MaxFactsPerSeed:           8,
		MaxBridgeFacts:            0,
		MaxSourceRefsPerGraphPath: 2,
	})
	if err != nil {
		t.Fatalf("ExpandGraphSources error = %v", err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want one budgeted candidate", result.Candidates)
	}
	if !containsGraphFactID(result.Candidates[0].FactIDs, "fact_tea") || !containsGraphFactID(result.Candidates[0].FactIDs, "fact_tea_again") {
		t.Fatalf("candidate fact ids = %v, want merged seed facts for same source", result.Candidates[0].FactIDs)
	}
}

func TestExpandGraphSourcesAddsBridgeCandidates(t *testing.T) {
	ctx := context.Background()
	store, scope := newGraphExpansionTestStore(t)
	putGraphTestEntity(t, ctx, store, scope, Entity{ID: "ent_ada", Type: EntityPerson, Name: "Ada"})
	putGraphTestEntity(t, ctx, store, scope, Entity{ID: "ent_ben", Type: EntityPerson, Name: "Ben"})
	putGraphTestEntity(t, ctx, store, scope, Entity{ID: "ent_club", Type: EntityOrganization, Name: "climbing club"})
	seedFact := putGraphTestFact(t, ctx, store, scope, Fact{
		ID:              "fact_ada_club",
		SubjectEntityID: "ent_ada",
		ObjectEntityIDs: []EntityID{"ent_club"},
		RelationType:    RelationActivity,
		FactText:        "Ada joined the climbing club.",
		SourceRefs:      []views.SourceRef{graphTestRef("conv", "dia-1")},
	})
	putGraphTestFact(t, ctx, store, scope, Fact{
		ID:              "fact_ben_club",
		SubjectEntityID: "ent_ben",
		ObjectEntityIDs: []EntityID{"ent_club"},
		RelationType:    RelationActivity,
		FactText:        "Ben practices at the climbing club.",
		SourceRefs:      []views.SourceRef{graphTestRef("conv", "dia-2")},
	})

	result, err := ExpandGraphSources(ctx, store, scope, []GraphSeedFact{{Fact: seedFact, Score: 1}}, GraphExpansionOptions{
		MaxCandidates:             4,
		MaxFactsPerSeed:           4,
		MaxBridgeFacts:            4,
		MaxSourceRefsPerGraphPath: 2,
	})
	if err != nil {
		t.Fatalf("ExpandGraphSources error = %v", err)
	}
	if !hasGraphOrigin(result.Candidates, GraphOriginBridge) {
		t.Fatalf("candidates = %+v, want bridge candidate", result.Candidates)
	}
}

func TestExpandGraphSourcesSkipsNonGraphableSeedFacts(t *testing.T) {
	ctx := context.Background()
	store, scope := newGraphExpansionTestStore(t)
	putGraphTestEntity(t, ctx, store, scope, Entity{ID: "ent_ada", Type: EntityPerson, Name: "Ada"})
	putGraphTestEntity(t, ctx, store, scope, Entity{ID: "ent_tea", Type: EntityObject, Name: "tea"})
	graphableFact := putGraphTestFact(t, ctx, store, scope, Fact{
		ID:              "fact_tea",
		SubjectEntityID: "ent_ada",
		ObjectEntityIDs: []EntityID{"ent_tea"},
		RelationType:    RelationPreference,
		FactText:        "Ada likes tea.",
		SourceRefs:      []views.SourceRef{graphTestRef("conv", "dia-1")},
	})
	nonGraphableFact := putGraphTestFact(t, ctx, store, scope, Fact{
		ID:              "fact_unary",
		SubjectEntityID: "ent_ada",
		RelationType:    RelationPreference,
		FactText:        "Ada mentioned tea.",
		SourceRefs:      []views.SourceRef{graphTestRef("conv", "dia-2")},
		Metadata:        map[string]any{FactGraphableMetadataKey: false},
	})

	result, err := ExpandGraphSources(ctx, store, scope, []GraphSeedFact{
		{Fact: nonGraphableFact, Score: 1},
		{Fact: graphableFact, Score: 0.8},
	}, GraphExpansionOptions{
		MaxCandidates:             4,
		MaxFactsPerSeed:           4,
		MaxBridgeFacts:            0,
		MaxSourceRefsPerGraphPath: 2,
	})
	if err != nil {
		t.Fatalf("ExpandGraphSources error = %v", err)
	}
	if len(result.Seeds) != 1 || result.Seeds[0].Fact.ID != "fact_tea" {
		t.Fatalf("seeds = %+v, want only graphable seed", result.Seeds)
	}
	for _, candidate := range result.Candidates {
		if containsGraphFactID(candidate.FactIDs, "fact_unary") {
			t.Fatalf("candidate fact ids = %v, want non-graphable fact skipped", candidate.FactIDs)
		}
	}
}

func newGraphExpansionTestStore(t *testing.T) (*WorkspaceStore, views.Scope) {
	t.Helper()
	return NewWorkspaceStore(sdkworkspace.NewMemWorkspace()), views.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
}

func putGraphTestEntity(t *testing.T, ctx context.Context, store *WorkspaceStore, scope views.Scope, entity Entity) {
	t.Helper()
	entity.Scope = scope
	if entity.Confidence == 0 {
		entity.Confidence = 0.9
	}
	if _, err := store.PutEntity(ctx, entity); err != nil {
		t.Fatalf("PutEntity(%s): %v", entity.ID, err)
	}
}

func putGraphTestFact(t *testing.T, ctx context.Context, store *WorkspaceStore, scope views.Scope, fact Fact) Fact {
	t.Helper()
	fact.Scope = scope
	if fact.Confidence == 0 {
		fact.Confidence = 0.9
	}
	stored, err := store.PutFact(ctx, fact)
	if err != nil {
		t.Fatalf("PutFact(%s): %v", fact.ID, err)
	}
	return stored
}

func graphTestRef(conversationID, messageID string) views.SourceRef {
	return views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: conversationID,
			MessageID:      messageID,
		},
	}
}

func hasGraphOrigin(candidates []GraphSourceCandidate, origin string) bool {
	for _, candidate := range candidates {
		if candidate.Origin == origin {
			return true
		}
	}
	return false
}

func containsGraphFactID(ids []FactID, id FactID) bool {
	for _, existing := range ids {
		if existing == id {
			return true
		}
	}
	return false
}
