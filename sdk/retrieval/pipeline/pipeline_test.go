package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestPipelineBM25Only(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "a", Content: "alpha bravo", Timestamp: now},
		{ID: "b", Content: "charlie delta", Timestamp: now},
	})
	pipe := New(
		Retrieve{Lane: "bm25", Spec: RetrieveSpec{Mode: ModeBM25, TopK: 10}},
		RRFFusion{K: 60},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{QueryText: "alpha", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "a" {
		t.Fatalf("hits=%+v", resp.Hits)
	}
}

func TestPipelineMultiRetrieveAndRRF(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "1", Content: "coffee tea", Vector: []float32{1, 0, 0}, Timestamp: time.Now()},
		{ID: "2", Content: "unrelated", Vector: []float32{0, 1, 0}, Timestamp: time.Now()},
	})
	pipe := New(
		MultiRetrieve{
			"bm25":   {Mode: ModeBM25, TopK: 10},
			"vector": {Mode: ModeVector, TopK: 10},
		},
		RRFFusion{K: 60},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
		QueryText: "coffee", QueryVector: []float32{1, 0, 0}, TopK: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) < 1 || resp.Hits[0].Doc.ID != "1" {
		t.Fatalf("hits=%+v", resp.Hits)
	}
}

func TestPipelineReturnRawIncludesRetrieverHits(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "1", Content: "coffee tea", Vector: []float32{1, 0, 0}, Timestamp: time.Now()},
		{ID: "2", Content: "unrelated", Vector: []float32{0, 1, 0}, Timestamp: time.Now()},
	})
	pipe := New(
		MultiRetrieve{
			"bm25":   {Mode: ModeBM25, TopK: 10},
			"vector": {Mode: ModeVector, TopK: 10},
		},
		RRFFusion{K: 60},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
		QueryText:   "coffee",
		QueryVector: []float32{1, 0, 0},
		TopK:        5,
		ReturnRaw:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.RawByRetriever) != 2 {
		t.Fatalf("expected raw hits for 2 retrievers, got %+v", resp.RawByRetriever)
	}
	if len(resp.RawByRetriever["bm25"]) == 0 {
		t.Fatalf("expected bm25 raw hits, got %+v", resp.RawByRetriever)
	}
	if len(resp.RawByRetriever["vector"]) == 0 {
		t.Fatalf("expected vector raw hits, got %+v", resp.RawByRetriever)
	}
	if resp.Execution == nil {
		t.Fatal("expected Execution to be populated when ReturnRaw=true")
	}
	if len(resp.Execution.Lanes) != 2 {
		t.Fatalf("expected 2 lanes in Execution, got %+v", resp.Execution.Lanes)
	}
	for _, lane := range resp.Execution.Lanes {
		raw, ok := resp.RawByRetriever[string(lane.Key)]
		if !ok {
			t.Fatalf("lane %q missing in RawByRetriever projection", lane.Key)
		}
		if len(raw) != len(lane.Hits) {
			t.Fatalf("lane %q raw/exec mismatch: raw=%d exec=%d", lane.Key, len(raw), len(lane.Hits))
		}
	}
}

func TestPipelineDebugIncludeStagesRecordsTrace(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "1", Content: "coffee tea", Timestamp: time.Now()},
	})
	pipe := New(
		Retrieve{Lane: "bm25", Spec: RetrieveSpec{Mode: ModeBM25, TopK: 10}},
		RRFFusion{K: 60},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
		QueryText: "coffee",
		TopK:      5,
		Debug:     retrieval.SearchDebug{IncludeStages: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Execution == nil {
		t.Fatal("expected Execution when Debug.IncludeStages=true")
	}
	if len(resp.Execution.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %+v", resp.Execution.Stages)
	}
	if resp.Execution.Stages[0].Name == "" || resp.Execution.Stages[0].Took <= 0 {
		t.Fatalf("first stage missing name/duration: %+v", resp.Execution.Stages[0])
	}
	if len(resp.Execution.Lanes) != 0 {
		t.Fatalf("expected no lanes when IncludeLanes=false, got %+v", resp.Execution.Lanes)
	}
	if resp.RawByRetriever != nil {
		t.Fatalf("RawByRetriever should remain nil when ReturnRaw=false, got %+v", resp.RawByRetriever)
	}
}

func TestPipelineDebugIncludeLanesWithoutLegacyProjection(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "1", Content: "coffee tea", Timestamp: time.Now()},
	})
	pipe := New(
		Retrieve{Lane: "bm25", Spec: RetrieveSpec{Mode: ModeBM25, TopK: 10}},
		RRFFusion{K: 60},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
		QueryText: "coffee",
		TopK:      5,
		Debug:     retrieval.SearchDebug{IncludeLanes: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Execution == nil || len(resp.Execution.Lanes) != 1 {
		t.Fatalf("expected one lane in Execution, got %+v", resp.Execution)
	}
	if resp.RawByRetriever != nil {
		t.Fatalf("RawByRetriever should only be populated by ReturnRaw, got %+v", resp.RawByRetriever)
	}
}

func TestEntityBoost(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "a", Content: "alice loves coffee", Metadata: map[string]any{"entities": []string{"alice"}}, Timestamp: time.Now()},
		{ID: "b", Content: "bob loves coffee", Metadata: map[string]any{"entities": []string{"bob"}}, Timestamp: time.Now()},
	})
	pipe := New(
		EntityExtract{LLMExtractor: func(_ context.Context, _ string) ([]string, error) {
			return []string{"alice"}, nil
		}},
		MultiRetrieve{"bm25": {Mode: ModeBM25, TopK: 10}},
		RRFFusion{K: 60},
		EntityBoost{Boost: 0.5},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{QueryText: "loves coffee", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) < 2 || resp.Hits[0].Doc.ID != "a" {
		t.Fatalf("expected a first, got %+v", resp.Hits)
	}
}

func TestTimeDecayBringsNewerToTop(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "old", Content: "user likes coffee", Timestamp: now.AddDate(0, -6, 0)},
		{ID: "new", Content: "user likes coffee", Timestamp: now.AddDate(0, 0, -1)},
	})
	pipe := New(
		Retrieve{Lane: "bm25", Spec: RetrieveSpec{Mode: ModeBM25, TopK: 10}},
		RRFFusion{K: 60},
		TimeDecay{HalfLife: 30 * 24 * time.Hour, Now: func() time.Time { return now }},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{QueryText: "coffee", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) < 2 || resp.Hits[0].Doc.ID != "new" {
		t.Fatalf("expected new on top, got %+v", resp.Hits)
	}
}

func TestPipelineLaneResultCarriesTook(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "1", Content: "coffee tea", Vector: []float32{1, 0, 0}, Timestamp: time.Now()},
		{ID: "2", Content: "unrelated", Vector: []float32{0, 1, 0}, Timestamp: time.Now()},
	})
	pipe := New(
		MultiRetrieve{
			string(retrieval.LaneBM25):   {Mode: ModeBM25, TopK: 10},
			string(retrieval.LaneVector): {Mode: ModeVector, TopK: 10},
		},
		RRFFusion{K: 60},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
		QueryText:   "coffee",
		QueryVector: []float32{1, 0, 0},
		TopK:        5,
		Debug:       retrieval.SearchDebug{IncludeLanes: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Execution == nil || len(resp.Execution.Lanes) != 2 {
		t.Fatalf("expected 2 lanes in Execution, got %+v", resp.Execution)
	}
	for _, lane := range resp.Execution.Lanes {
		if lane.Took <= 0 {
			t.Fatalf("lane %q Took=%s, want > 0", lane.Key, lane.Took)
		}
	}
}

func TestPipelineLaneResultTookKeyedByLaneKey(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{{ID: "1", Content: "coffee", Timestamp: time.Now()}})
	pipe := New(
		Retrieve{Lane: string(retrieval.LaneBM25), Spec: RetrieveSpec{Mode: ModeBM25, TopK: 10}},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
		QueryText: "coffee", TopK: 5,
		Debug: retrieval.SearchDebug{IncludeLanes: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Execution == nil || len(resp.Execution.Lanes) != 1 {
		t.Fatalf("expected one lane, got %+v", resp.Execution)
	}
	lane := resp.Execution.Lanes[0]
	if lane.Key != retrieval.LaneBM25 {
		t.Fatalf("expected lane key %q, got %q", retrieval.LaneBM25, lane.Key)
	}
	if lane.Took <= 0 {
		t.Fatalf("expected Took > 0, got %s", lane.Took)
	}
}

func TestPipelineLaneOrderStable(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "1", Content: "coffee tea", Vector: []float32{1, 0, 0}, Timestamp: time.Now()},
	})
	pipe := New(
		MultiRetrieve{
			"zeta-custom":              {Mode: ModeBM25, TopK: 5},
			string(retrieval.LaneBM25): {Mode: ModeBM25, TopK: 5},
			"alpha-custom":             {Mode: ModeBM25, TopK: 5},
			string(retrieval.LaneVector): {
				Mode: ModeVector, TopK: 5,
			},
		},
		Limit{TopK: 10},
	)
	expect := []retrieval.LaneKey{
		retrieval.LaneBM25,
		retrieval.LaneVector,
		"alpha-custom",
		"zeta-custom",
	}
	for i := 0; i < 4; i++ {
		resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
			QueryText:   "coffee",
			QueryVector: []float32{1, 0, 0},
			TopK:        5,
			Debug:       retrieval.SearchDebug{IncludeLanes: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Execution == nil || len(resp.Execution.Lanes) != len(expect) {
			t.Fatalf("run %d: expected %d lanes, got %+v", i, len(expect), resp.Execution)
		}
		for j, lane := range resp.Execution.Lanes {
			if lane.Key != expect[j] {
				t.Fatalf("run %d: lane %d = %q, want %q", i, j, lane.Key, expect[j])
			}
		}
	}
}

type fakeHybridIndex struct {
	retrieval.Index
	gotDebug retrieval.SearchDebug
	resp     *retrieval.SearchResponse
}

func (f *fakeHybridIndex) Capabilities() retrieval.Capabilities {
	c := retrieval.DefaultMemoryCapabilities()
	c.Hybrid = true
	c.Debug = true
	return c
}

func (f *fakeHybridIndex) SearchHybrid(_ context.Context, _ string, req retrieval.HybridRequest) (*retrieval.SearchResponse, error) {
	f.gotDebug = req.Debug
	return f.resp, nil
}

func TestPipelineHybridShortCircuitForwardsDebug(t *testing.T) {
	hits := []retrieval.Hit{{Doc: retrieval.Doc{ID: "x"}, Score: 1.0}}
	exec := &retrieval.SearchExecution{
		Lanes: []retrieval.LaneResult{{Key: retrieval.LaneHybrid, Hits: hits, Took: time.Millisecond}},
		Stages: []retrieval.StageResult{
			{Name: "native.hybrid", HitsIn: 0, HitsOut: 1, Took: time.Millisecond},
		},
	}
	hybrid := &fakeHybridIndex{
		Index: memory.New(),
		resp:  &retrieval.SearchResponse{Hits: hits, Execution: exec},
	}
	pipe := New(HybridShortCircuit{}, Limit{TopK: 5})
	resp, err := pipe.Run(context.Background(), hybrid, "ns", retrieval.SearchRequest{
		QueryText: "q",
		TopK:      5,
		Debug:     retrieval.SearchDebug{IncludeLanes: true, IncludeStages: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hybrid.gotDebug.IncludeLanes || !hybrid.gotDebug.IncludeStages {
		t.Fatalf("backend did not receive Debug, got %+v", hybrid.gotDebug)
	}
	if resp.Execution == nil {
		t.Fatal("expected Execution to be merged from short-circuit response")
	}
	if len(resp.Execution.Lanes) != 1 || resp.Execution.Lanes[0].Key != retrieval.LaneHybrid {
		t.Fatalf("expected hybrid lane in Execution, got %+v", resp.Execution.Lanes)
	}
	foundNative := false
	for _, st := range resp.Execution.Stages {
		if st.Name == "native.hybrid" {
			foundNative = true
		}
	}
	if !foundNative {
		t.Fatalf("expected native stage in Execution.Stages, got %+v", resp.Execution.Stages)
	}
}

func TestPickFinalishDoesNotMutateRerankedOrFused(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "newer", Content: "user likes coffee", Timestamp: now},
		{ID: "older", Content: "user likes coffee", Timestamp: now.Add(-365 * 24 * time.Hour)},
	})
	st := &State{
		Index:     idx,
		Namespace: ns,
		Request:   &retrieval.SearchRequest{QueryText: "coffee", TopK: 5},
		Recalls:   map[string][]retrieval.Hit{},
	}
	if err := (Retrieve{Lane: "bm25", Spec: RetrieveSpec{Mode: ModeBM25, TopK: 10}}).Run(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := (RRFFusion{}).Run(ctx, st); err != nil {
		t.Fatal(err)
	}
	if len(st.Fused) == 0 {
		t.Fatalf("expected fused hits, got %+v", st.Fused)
	}
	beforeFusedScores := make([]float64, len(st.Fused))
	for i, h := range st.Fused {
		beforeFusedScores[i] = h.Score
	}
	if err := (TimeDecay{HalfLife: time.Hour, Now: func() time.Time { return now }}).Run(ctx, st); err != nil {
		t.Fatal(err)
	}
	for i, h := range st.Fused {
		if h.Score != beforeFusedScores[i] {
			t.Fatalf("fused score mutated at %d: was %f now %f", i, beforeFusedScores[i], h.Score)
		}
	}
}

func TestDedupAndPostFilter(t *testing.T) {
	ctx := context.Background()
	idx := memory.New()
	ns := "ns"
	now := time.Now()
	_ = idx.Upsert(ctx, ns, []retrieval.Doc{
		{ID: "k", Content: "foo bar", Metadata: map[string]any{"keep": true}, Timestamp: now},
		{ID: "d", Content: "foo bar", Metadata: map[string]any{"keep": false}, Timestamp: now},
	})
	pipe := New(
		MultiRetrieve{
			"a": {Mode: ModeBM25, TopK: 10},
			"b": {Mode: ModeBM25, TopK: 10},
		},
		RRFFusion{K: 60},
		Dedup{},
		PostFilter{},
		Limit{TopK: 5},
	)
	resp, err := pipe.Run(ctx, idx, ns, retrieval.SearchRequest{
		QueryText: "foo",
		Filter:    retrieval.Filter{Eq: map[string]any{"keep": true}},
		TopK:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].Doc.ID != "k" {
		t.Fatalf("hits=%+v", resp.Hits)
	}
}
