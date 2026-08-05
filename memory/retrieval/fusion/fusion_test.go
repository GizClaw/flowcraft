package fusion

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
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
	if _, err := fusor.SearchDetailed(ctx, component.SearchRequest{Scope: testScope(), Query: "q"}); !sdkmemory.IsKind(err, sdkmemory.KindOperationInterrupted) {
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
	if _, err := blockedFusor.SearchDetailed(ctx, component.SearchRequest{Scope: testScope(), Query: "q"}); !sdkmemory.IsKind(err, sdkmemory.KindOperationInterrupted) {
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
		Source: sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "source-" + id},
	}
}

func testScope() sdkmemory.Scope {
	return sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
}
