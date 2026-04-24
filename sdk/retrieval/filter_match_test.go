package retrieval

import "testing"

func TestDocMatchesFilter_NumericTypesAreInterchangeable(t *testing.T) {
	doc := Doc{
		ID: "n",
		Metadata: map[string]any{
			"score": float64(5),
			"count": int64(7),
			"ratio": float32(1.5),
		},
	}
	cases := []Filter{
		{Eq: map[string]any{"score": 5}},
		{Eq: map[string]any{"score": int64(5)}},
		{Eq: map[string]any{"count": float64(7)}},
		{Eq: map[string]any{"ratio": 1.5}},
		{In: map[string][]any{"score": {1, 5, 9}}},
		{Range: map[string]Range{"score": {Gte: int64(5), Lt: 6}}},
	}
	for i, f := range cases {
		if !DocMatchesFilter(doc, f) {
			t.Fatalf("case %d: expected match for %+v", i, f)
		}
	}
}

func TestDocMatchesFilter_NotComposesWithSiblingPredicates(t *testing.T) {
	doc := Doc{
		ID:      "doc1",
		Content: "hello",
		Metadata: map[string]any{
			"tenant": "other",
			"status": "active",
		},
	}

	filter := Filter{
		Not: &Filter{
			Eq: map[string]any{"status": "deleted"},
		},
		Eq: map[string]any{"tenant": "acme"},
	}

	if DocMatchesFilter(doc, filter) {
		t.Fatal("expected sibling Eq predicate to remain enforced alongside Not")
	}
}
