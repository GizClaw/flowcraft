package fusion

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

type searcherFunc func(context.Context, component.SearchRequest) ([]component.Candidate, error)

func (function searcherFunc) Search(ctx context.Context, request component.SearchRequest) ([]component.Candidate, error) {
	return function(ctx, request)
}

func TestCalibratorsBoundariesAndNaN(t *testing.T) {
	values, err := (MinMax{}).Calibrate(CalibrationInput{Scores: []float64{2, 4, 6}})
	if err != nil || values[0] != 0 || values[1] != 0.5 || values[2] != 1 {
		t.Fatalf("MinMax = %v, %v", values, err)
	}
	constant, err := (MinMax{}).Calibrate(CalibrationInput{Scores: []float64{3}})
	if err != nil || constant[0] != 1 {
		t.Fatalf("constant MinMax = %v, %v", constant, err)
	}
	logistic, err := (Logistic{Slope: 1}).Calibrate(CalibrationInput{Scores: []float64{0}})
	if err != nil || logistic[0] != 0.5 {
		t.Fatalf("Logistic = %v, %v", logistic, err)
	}
	saturated, err := (Saturating{Scale: 2}).Calibrate(CalibrationInput{Scores: []float64{0, 2}})
	if err != nil || saturated[0] != 0 || saturated[1] != 0.5 {
		t.Fatalf("Saturating = %v, %v", saturated, err)
	}
	if _, err := (MinMax{}).Calibrate(CalibrationInput{Scores: []float64{math.NaN()}}); err == nil {
		t.Fatal("MinMax accepted NaN")
	}
}

func TestCosineCalibrationBoundariesNaNAndFloor(t *testing.T) {
	values, err := (Cosine{}).Calibrate(CalibrationInput{Scores: []float64{-2, -1, 0, 1, 2}})
	if err != nil || !equalScores(values, []float64{0, 0, .5, 1, 1}) {
		t.Fatalf("cosine = %v, %v", values, err)
	}
	values, err = (Cosine{FloorEnabled: true, SemanticFloor: .2}).Calibrate(
		CalibrationInput{Scores: []float64{.19, .2}},
	)
	if err != nil || !equalScores(values, []float64{0, .6}) {
		t.Fatalf("floored cosine = %v, %v", values, err)
	}
	if _, err := (Cosine{}).Calibrate(CalibrationInput{Scores: []float64{math.NaN()}}); err == nil {
		t.Fatal("cosine accepted NaN")
	}
}

func TestBM25QueryAdaptiveSigmoidFixtures(t *testing.T) {
	calibrator := BM25QuerySigmoid{}
	short, err := calibrator.Calibrate(CalibrationInput{Query: "one two", Scores: []float64{5}})
	if err != nil || short[0] != .5 {
		t.Fatalf("short = %v, %v", short, err)
	}
	long, err := calibrator.Calibrate(CalibrationInput{
		Query:  "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen",
		Scores: []float64{12},
	})
	if err != nil || long[0] != .5 {
		t.Fatalf("long = %v, %v", long, err)
	}
	empty, err := calibrator.Calibrate(CalibrationInput{Scores: []float64{99}})
	if err != nil || empty[0] != 0 {
		t.Fatalf("empty = %v, %v", empty, err)
	}
}

func TestFusionRenormalizesFailuresAndDeduplicates(t *testing.T) {
	failing := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return nil, errors.New("offline")
	})
	laneA := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{candidate("same", 10), candidate("same", 10), candidate("a", 5)}, nil
	})
	laneB := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{candidate("same", 2), candidate("b", 1)}, nil
	})
	fusor, err := New([]Lane{
		{Name: "failed", Searcher: failing, Weight: 100, Calibrator: MinMax{}},
		{Name: "a", Searcher: laneA, Weight: 1, Calibrator: MinMax{}},
		{Name: "b", Searcher: laneB, Weight: 3, Calibrator: MinMax{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := fusor.SearchDetailed(context.Background(), component.SearchRequest{Scope: testScope(), Query: "q"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 1 || len(result.Candidates) != 3 {
		t.Fatalf("result = %+v", result)
	}
	if result.Candidates[0].ID != "same" || result.Candidates[0].Score != 1 {
		t.Fatalf("top candidate = %+v", result.Candidates[0])
	}
	explanation := result.Candidates[0].Explanation.Terms
	if len(explanation) != 2 || explanation[0].Lane != "a" || explanation[0].Raw != 10 ||
		explanation[0].Calibrated != 1 || explanation[0].Weight != .25 ||
		explanation[0].Contribution != .25 || explanation[0].CalibrationVersion == "" ||
		explanation[1].Lane != "b" || explanation[1].Contribution != .75 {
		t.Fatalf("explanation = %+v", explanation)
	}
	if result.Candidates[1].ID != "a" || result.Candidates[1].Score != 0 {
		t.Fatalf("stable tie = %+v", result.Candidates)
	}
}

func TestFusionOneAndTwoLaneFailure(t *testing.T) {
	success := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{candidate("only", 7)}, nil
	})
	fail := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return nil, errors.New("failed")
	})
	for _, lanes := range [][]Lane{
		{
			{Name: "ok1", Searcher: success, Weight: 1, Calibrator: MinMax{}},
			{Name: "ok2", Searcher: success, Weight: 1, Calibrator: MinMax{}},
			{Name: "bad", Searcher: fail, Weight: 9, Calibrator: MinMax{}},
		},
		{
			{Name: "ok", Searcher: success, Weight: 1, Calibrator: MinMax{}},
			{Name: "bad1", Searcher: fail, Weight: 9, Calibrator: MinMax{}},
			{Name: "bad2", Searcher: fail, Weight: 9, Calibrator: MinMax{}},
		},
	} {
		fusor, err := New(lanes)
		if err != nil {
			t.Fatal(err)
		}
		result, err := fusor.SearchDetailed(context.Background(), component.SearchRequest{Scope: testScope(), Query: "q"})
		if err != nil || len(result.Candidates) != 1 || result.Candidates[0].Score != 1 {
			t.Fatalf("result = %+v, %v", result, err)
		}
	}
}

// TestRRFPrefersCandidatesRankedByMultipleLanes pins the core RRF property:
// a candidate two lanes rank highly beats candidates that only one lane
// ranks highly, even when its native scores are lower.
func TestRRFPrefersCandidatesRankedByMultipleLanes(t *testing.T) {
	laneA := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{candidate("shared", 3), candidate("only-a", 100)}, nil
	})
	laneB := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{candidate("only-b", 90), candidate("shared", 1)}, nil
	})
	fusor, err := NewWithOptions([]Lane{
		{Name: "a", Searcher: laneA, Weight: 1, Calibrator: MinMax{}},
		{Name: "b", Searcher: laneB, Weight: 1, Calibrator: MinMax{}},
	}, Options{Mode: ModeRRF})
	if err != nil {
		t.Fatal(err)
	}
	result, err := fusor.SearchDetailed(context.Background(), component.SearchRequest{Scope: testScope(), Query: "q"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 3 || result.Candidates[0].ID != "shared" {
		t.Fatalf("candidates = %+v", result.Candidates)
	}
	if len(result.Candidates[0].Explanation.Terms) != 2 ||
		result.Candidates[0].Explanation.Terms[0].CalibrationVersion != RRFAlgorithmVersion {
		t.Fatalf("explanation = %+v", result.Candidates[0].Explanation.Terms)
	}
	// Candidates that every lane ranks first normalize to 1.
	bothFirst := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{candidate("top", 5)}, nil
	})
	fusor, err = NewWithOptions([]Lane{
		{Name: "a", Searcher: bothFirst, Weight: 1, Calibrator: MinMax{}},
		{Name: "b", Searcher: bothFirst, Weight: 1, Calibrator: MinMax{}},
	}, Options{Mode: ModeRRF})
	if err != nil {
		t.Fatal(err)
	}
	result, err = fusor.SearchDetailed(context.Background(), component.SearchRequest{Scope: testScope(), Query: "q"})
	if err != nil || len(result.Candidates) != 1 || result.Candidates[0].Score != 1 {
		t.Fatalf("normalized rrf score = %+v, %v", result, err)
	}
}

// TestRRFSkipsFailedLanesAndKeepsWeightedDefault pins that failed lanes only
// drop their own contribution, and that New keeps the weighted algorithm for
// callers that do not opt in.
func TestRRFSkipsFailedLanesAndKeepsWeightedDefault(t *testing.T) {
	success := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{candidate("only", 7)}, nil
	})
	failing := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return nil, errors.New("offline")
	})
	fusor, err := NewWithOptions([]Lane{
		{Name: "ok", Searcher: success, Weight: 1, Calibrator: MinMax{}},
		{Name: "bad", Searcher: failing, Weight: 9, Calibrator: MinMax{}},
	}, Options{Mode: ModeRRF})
	if err != nil {
		t.Fatal(err)
	}
	result, err := fusor.SearchDetailed(context.Background(), component.SearchRequest{Scope: testScope(), Query: "q"})
	if err != nil || len(result.Candidates) != 1 || result.Candidates[0].Score != 1 || len(result.Diagnostics) != 1 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	weighted, err := New([]Lane{{Name: "ok", Searcher: success, Weight: 1, Calibrator: MinMax{}}})
	if err != nil {
		t.Fatal(err)
	}
	if weighted.mode != ModeWeighted {
		t.Fatalf("New mode = %q, want weighted", weighted.mode)
	}
	if _, err := NewWithOptions([]Lane{{Name: "ok", Searcher: success, Weight: 1, Calibrator: MinMax{}}}, Options{Mode: "unknown"}); err == nil {
		t.Fatal("unknown fusion mode accepted")
	}
}

func TestFusionAllFailedAndContextCancel(t *testing.T) {
	fail := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return nil, errors.New("failed")
	})
	fusor, err := New([]Lane{{Name: "bad", Searcher: fail, Weight: 1, Calibrator: MinMax{}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := fusor.SearchDetailed(context.Background(), component.SearchRequest{Scope: testScope(), Query: "q"})
	if err != nil || len(result.Candidates) != 0 || len(result.Diagnostics) != 1 {
		t.Fatalf("all-failed result = %+v, %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fusor.SearchDetailed(ctx, component.SearchRequest{Scope: testScope(), Query: "q"}); !corememory.IsKind(err, corememory.KindOperationInterrupted) {
		t.Fatalf("cancel error = %v", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	blocking := searcherFunc(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		cancel()
		<-release // Deliberately ignores the caller context.
		return nil, nil
	})
	blockedFusor, err := New([]Lane{{Name: "blocking", Searcher: blocking, Weight: 1, Calibrator: MinMax{}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blockedFusor.SearchDetailed(ctx, component.SearchRequest{Scope: testScope(), Query: "q"}); !corememory.IsKind(err, corememory.KindOperationInterrupted) {
		t.Fatalf("blocking cancel error = %v", err)
	}
}

func equalScores(left, right []float64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func candidate(id string, score float64) component.Candidate {
	return component.Candidate{
		ID: id, Lane: "native", Name: "fact", Score: score,
		Source: corememory.SourceRef{Kind: corememory.SourceMessage, ID: "source-" + id},
	}
}

func testScope() corememory.Scope {
	return corememory.Scope{RuntimeID: "runtime", UserID: "user"}
}
