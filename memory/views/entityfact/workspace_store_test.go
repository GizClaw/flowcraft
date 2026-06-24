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
	if _, err := store.PutEntity(ctx, Entity{
		ID:    "ent_tea",
		Scope: scope,
		Type:  EntityObject,
		Name:  "tea",
	}); err != nil {
		t.Fatalf("PutEntity object error = %v", err)
	}

	fact, err := store.PutFact(ctx, Fact{
		ID:              "fact_tea",
		Scope:           scope,
		SubjectEntityID: "ent_ada",
		ObjectEntityIDs: []EntityID{"ent_tea"},
		RelationType:    RelationPreference,
		FactText:        "Ada likes tea.",
		TimeText:        "Tuesday morning",
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
	queries := map[string]func() ([]Fact, error){
		"entity subject": func() ([]Fact, error) { return store.ListFactsByEntity(ctx, scope, "ent_ada", ListOptions{}) },
		"entity object":  func() ([]Fact, error) { return store.ListFactsByEntity(ctx, scope, "ent_tea", ListOptions{}) },
		"subject":        func() ([]Fact, error) { return store.ListFactsBySubject(ctx, scope, "ent_ada", ListOptions{}) },
		"object":         func() ([]Fact, error) { return store.ListFactsByObject(ctx, scope, "ent_tea", ListOptions{}) },
		"relation": func() ([]Fact, error) {
			return store.ListFactsByRelation(ctx, scope, RelationPreference, ListOptions{})
		},
		"time": func() ([]Fact, error) { return store.ListFactsByTime(ctx, scope, " tuesday   morning ", ListOptions{}) },
		"source": func() ([]Fact, error) {
			return store.ListFactsBySourceMessage(ctx, scope, "conv", "dia-1", ListOptions{})
		},
	}
	for name, query := range queries {
		gotFacts, err := query()
		if err != nil {
			t.Fatalf("%s query error = %v", name, err)
		}
		if len(gotFacts) != 1 || gotFacts[0].ID != "fact_tea" {
			t.Fatalf("%s facts = %+v, want fact_tea", name, gotFacts)
		}
	}

	if err := store.ws.RemoveAll(ctx, store.factIndexDir(scope)); err != nil {
		t.Fatalf("remove fact index: %v", err)
	}
	fallbackFacts, err := store.ListFactsByEntity(ctx, scope, "ent_tea", ListOptions{})
	if err != nil {
		t.Fatalf("ListFactsByEntity fallback error = %v", err)
	}
	if len(fallbackFacts) != 1 || fallbackFacts[0].ID != "fact_tea" {
		t.Fatalf("fallback facts = %+v, want fact_tea", fallbackFacts)
	}
}
