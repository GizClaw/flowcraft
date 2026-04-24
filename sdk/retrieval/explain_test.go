package retrieval

import "testing"

func TestProjectRawByRetrieverNilExecution(t *testing.T) {
	if got := ProjectRawByRetriever(nil); got != nil {
		t.Fatalf("expected nil for nil execution, got %+v", got)
	}
	if got := ProjectRawByRetriever(&SearchExecution{}); got != nil {
		t.Fatalf("expected nil for empty execution, got %+v", got)
	}
}

func TestProjectRawByRetrieverCopiesLaneHits(t *testing.T) {
	hits := []Hit{{Doc: Doc{ID: "a"}}, {Doc: Doc{ID: "b"}}}
	exec := &SearchExecution{
		Lanes: []LaneResult{{Key: LaneBM25, Hits: hits}},
	}
	got := ProjectRawByRetriever(exec)
	if len(got) != 1 || len(got["bm25"]) != 2 {
		t.Fatalf("unexpected projection: %+v", got)
	}
	got["bm25"][0].Doc.ID = "MUTATED"
	if hits[0].Doc.ID != "a" {
		t.Fatalf("projection should not alias source slice; src now %s", hits[0].Doc.ID)
	}
}

func TestProjectRawByRetrieverSkipsEmptyLanes(t *testing.T) {
	exec := &SearchExecution{
		Lanes: []LaneResult{
			{Key: LaneBM25},
			{Key: "", Hits: []Hit{{Doc: Doc{ID: "x"}}}},
		},
	}
	if got := ProjectRawByRetriever(exec); got != nil {
		t.Fatalf("expected nil when no lane has both key and hits, got %+v", got)
	}
}
