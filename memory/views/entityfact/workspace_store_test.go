package entityfact

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/views"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestWorkspaceStoreRoundTripsEntitiesFactsAndAliases(t *testing.T) {
	ctx := context.Background()
	store := NewWorkspaceStore(sdkworkspace.NewMemWorkspace())
	scope := views.Scope{RuntimeID: "rt", UserID: "user", ConversationID: "conv"}
	ref := views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: "conv",
			MessageID:      "dia-1",
		},
	}

	entity, err := store.PutEntity(ctx, Entity{
		ID:          "ent_ada",
		Scope:       scope,
		Type:        EntityPerson,
		Name:        "Ada Lovelace",
		Aliases:     []string{"Ada"},
		MentionRefs: []views.SourceRef{ref},
	})
	if err != nil {
		t.Fatalf("PutEntity error = %v", err)
	}
	if entity.CreatedAt.IsZero() || entity.UpdatedAt.IsZero() {
		t.Fatalf("entity timestamps were not populated: %+v", entity)
	}
	ids, err := store.LookupAlias(ctx, scope, " ada ")
	if err != nil {
		t.Fatalf("LookupAlias error = %v", err)
	}
	if len(ids) != 1 || ids[0] != "ent_ada" {
		t.Fatalf("LookupAlias ids = %v, want ent_ada", ids)
	}

	fact, err := store.PutFact(ctx, Fact{
		ID:              "fact_tea",
		Scope:           scope,
		SubjectEntityID: "ent_ada",
		RelationType:    RelationPreference,
		FactText:        "Ada likes tea.",
		SourceRefs:      []views.SourceRef{ref},
	})
	if err != nil {
		t.Fatalf("PutFact error = %v", err)
	}
	if fact.CreatedAt.IsZero() || fact.UpdatedAt.IsZero() {
		t.Fatalf("fact timestamps were not populated: %+v", fact)
	}

	facts, err := store.ListFacts(ctx, scope, ListOptions{})
	if err != nil {
		t.Fatalf("ListFacts error = %v", err)
	}
	if len(facts) != 1 || facts[0].ID != "fact_tea" {
		t.Fatalf("ListFacts = %+v, want fact_tea", facts)
	}
}
