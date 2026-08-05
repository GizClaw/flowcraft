// Package fusion runs independent retrieval lanes and fuses calibrated scores.
package fusion

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/internal/textutil"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const (
	AlgorithmVersion         = "weighted-sum-v1"
	CosineCalibrationVersion = "cosine-v1"
	BM25CalibrationVersion   = "bm25-query-sigmoid-v1"
)

type CalibrationInput struct {
	Query  string
	Scores []float64
}

// Calibrator maps lane-native scores into portable [0,1] values without
// depending on the current candidate set.
type Calibrator interface {
	Calibrate(CalibrationInput) ([]float64, error)
	Version() string
}

// MinMax maps (x-min)/(max-min). A finite constant lane maps to 1 so a
// one-result lane remains useful.
type MinMax struct{}

func (MinMax) Version() string { return "query-minmax-v1" }

func (MinMax) Calibrate(input CalibrationInput) ([]float64, error) {
	scores := input.Scores
	if err := finiteScores(scores); err != nil {
		return nil, err
	}
	if len(scores) == 0 {
		return []float64{}, nil
	}
	minimum, maximum := scores[0], scores[0]
	for _, score := range scores[1:] {
		minimum = min(minimum, score)
		maximum = max(maximum, score)
	}
	result := make([]float64, len(scores))
	if maximum == minimum {
		for i := range result {
			result[i] = 1
		}
		return result, nil
	}
	for i, score := range scores {
		result[i] = (score - minimum) / (maximum - minimum)
	}
	return result, nil
}

// Logistic maps x to 1/(1+exp(-Slope*(x-Midpoint))).
type Logistic struct {
	Slope    float64
	Midpoint float64
}

func (Logistic) Version() string { return "logistic-v1" }

func (calibrator Logistic) Calibrate(input CalibrationInput) ([]float64, error) {
	scores := input.Scores
	if err := finiteScores(scores); err != nil {
		return nil, err
	}
	if math.IsNaN(calibrator.Slope) || math.IsInf(calibrator.Slope, 0) || calibrator.Slope <= 0 ||
		math.IsNaN(calibrator.Midpoint) || math.IsInf(calibrator.Midpoint, 0) {
		return nil, errors.New("fusion: logistic slope must be positive and parameters finite")
	}
	result := make([]float64, len(scores))
	for i, score := range scores {
		result[i] = 1 / (1 + math.Exp(-calibrator.Slope*(score-calibrator.Midpoint)))
	}
	return result, nil
}

// Saturating maps non-negative x to x/(x+Scale).
type Saturating struct{ Scale float64 }

func (Saturating) Version() string { return "saturating-v1" }

func (calibrator Saturating) Calibrate(input CalibrationInput) ([]float64, error) {
	scores := input.Scores
	if err := finiteScores(scores); err != nil {
		return nil, err
	}
	if math.IsNaN(calibrator.Scale) || math.IsInf(calibrator.Scale, 0) || calibrator.Scale <= 0 {
		return nil, errors.New("fusion: saturation scale must be finite and positive")
	}
	result := make([]float64, len(scores))
	for i, score := range scores {
		if score < 0 {
			return nil, errors.New("fusion: saturation input must be non-negative")
		}
		result[i] = score / (score + calibrator.Scale)
	}
	return result, nil
}

type Identity struct{ CalibrationVersion string }

func (calibrator Identity) Version() string {
	if calibrator.CalibrationVersion == "" {
		return "identity-v1"
	}
	return calibrator.CalibrationVersion
}

func (calibrator Identity) Calibrate(input CalibrationInput) ([]float64, error) {
	if err := finiteScores(input.Scores); err != nil {
		return nil, err
	}
	result := append([]float64(nil), input.Scores...)
	for _, score := range result {
		if score < 0 || score > 1 {
			return nil, errors.New("fusion: identity input must be in [0,1]")
		}
	}
	return result, nil
}

// Cosine maps a clamped cosine from [-1,1] to [0,1]. When FloorEnabled is
// true, SemanticFloor is applied to the raw clamped cosine first and values
// below it map to zero. NaN and infinities are rejected.
type Cosine struct {
	FloorEnabled  bool
	SemanticFloor float64
}

func (Cosine) Version() string { return CosineCalibrationVersion }

func (calibrator Cosine) Calibrate(input CalibrationInput) ([]float64, error) {
	if math.IsNaN(calibrator.SemanticFloor) || math.IsInf(calibrator.SemanticFloor, 0) ||
		calibrator.SemanticFloor < -1 || calibrator.SemanticFloor > 1 {
		return nil, errors.New("fusion: semantic floor must be finite and in [-1,1]")
	}
	if err := finiteScores(input.Scores); err != nil {
		return nil, err
	}
	result := make([]float64, len(input.Scores))
	for i, score := range input.Scores {
		score = max(-1, min(1, score))
		if calibrator.FloorEnabled && score < calibrator.SemanticFloor {
			result[i] = 0
			continue
		}
		result[i] = (score + 1) / 2
	}
	return result, nil
}

// BM25QuerySigmoid uses the researched query-length buckets. Midpoint is the
// raw BM25 score yielding 0.5; Steepness controls the sigmoid slope.
type BM25QuerySigmoid struct{}

func (BM25QuerySigmoid) Version() string { return BM25CalibrationVersion }

func (BM25QuerySigmoid) Calibrate(input CalibrationInput) ([]float64, error) {
	if err := finiteScores(input.Scores); err != nil {
		return nil, err
	}
	queryLength := len(textutil.Tokens(input.Query))
	if queryLength == 0 {
		return make([]float64, len(input.Scores)), nil
	}
	midpoint, steepness := bm25Parameters(queryLength)
	result := make([]float64, len(input.Scores))
	for i, score := range input.Scores {
		result[i] = stableSigmoid(steepness * (score - midpoint))
	}
	return result, nil
}

func bm25Parameters(queryLength int) (float64, float64) {
	switch {
	case queryLength <= 3:
		return 5, 0.7
	case queryLength <= 6:
		return 7, 0.6
	case queryLength <= 9:
		return 9, 0.5
	case queryLength <= 15:
		return 10, 0.5
	default:
		return 12, 0.5
	}
}

func stableSigmoid(value float64) float64 {
	if value >= 0 {
		z := math.Exp(-value)
		return 1 / (1 + z)
	}
	z := math.Exp(value)
	return z / (1 + z)
}

type Lane struct {
	Name       string
	Searcher   component.Searcher
	Weight     float64
	Calibrator Calibrator
}

type Diagnostic struct {
	Lane string
	Err  error
}

type Result struct {
	Candidates  []component.Candidate
	Diagnostics []Diagnostic
}

type Fusion struct{ lanes []Lane }

var _ component.Searcher = (*Fusion)(nil)

func New(lanes []Lane) (*Fusion, error) {
	if len(lanes) == 0 {
		return nil, errors.New("fusion: at least one lane is required")
	}
	owned := append([]Lane(nil), lanes...)
	seen := make(map[string]struct{}, len(owned))
	for i, lane := range owned {
		if strings.TrimSpace(lane.Name) == "" || lane.Searcher == nil || lane.Calibrator == nil {
			return nil, fmt.Errorf("fusion: lane %d is incomplete", i)
		}
		if math.IsNaN(lane.Weight) || math.IsInf(lane.Weight, 0) || lane.Weight <= 0 {
			return nil, fmt.Errorf("fusion: lane %q weight must be finite and positive", lane.Name)
		}
		if _, ok := seen[lane.Name]; ok {
			return nil, fmt.Errorf("fusion: duplicate lane %q", lane.Name)
		}
		seen[lane.Name] = struct{}{}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })
	return &Fusion{lanes: owned}, nil
}

func (fusion *Fusion) Search(ctx context.Context, request component.SearchRequest) ([]component.Candidate, error) {
	result, err := fusion.SearchDetailed(ctx, request)
	return result.Candidates, err
}

// SearchDetailed uses weighted-sum fusion. Only successful lanes participate
// in weight normalization; absent candidates contribute zero.
func (fusion *Fusion) SearchDetailed(ctx context.Context, request component.SearchRequest) (Result, error) {
	if fusion == nil || len(fusion.lanes) == 0 {
		return Result{}, errors.New("fusion: lanes are required")
	}
	if ctx == nil {
		return Result{}, errors.New("fusion: context is required")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, sdkmemory.NewError(sdkmemory.KindOperationInterrupted, "context", err)
	}
	if err := request.Scope.Validate(); err != nil {
		return Result{}, sdkmemory.NewError(sdkmemory.KindInvalidRequest, "context", err)
	}
	if strings.TrimSpace(request.Query) == "" || request.Limit < 0 {
		return Result{}, sdkmemory.NewError(
			sdkmemory.KindInvalidRequest, "context", errors.New("fusion: query is required and limit must not be negative"),
		)
	}
	type laneResult struct {
		index      int
		candidates []component.Candidate
		err        error
	}
	results := make([]laneResult, len(fusion.lanes))
	completed := make(chan laneResult, len(fusion.lanes))
	for i, lane := range fusion.lanes {
		go func(index int, current Lane) {
			candidates, err := current.Searcher.Search(ctx, request)
			completed <- laneResult{index: index, candidates: candidates, err: err}
		}(i, lane)
	}
	for range fusion.lanes {
		select {
		case <-ctx.Done():
			return Result{}, sdkmemory.NewError(sdkmemory.KindOperationInterrupted, "context", ctx.Err())
		case result := <-completed:
			results[result.index] = result
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, sdkmemory.NewError(sdkmemory.KindOperationInterrupted, "context", err)
	}

	diagnostics := make([]Diagnostic, 0)
	successWeight := 0.0
	successCount := 0
	for i, result := range results {
		if result.err != nil {
			diagnostics = append(diagnostics, Diagnostic{Lane: fusion.lanes[i].Name, Err: result.err})
			continue
		}
		successCount++
		successWeight += fusion.lanes[i].Weight
	}
	if successCount == 0 {
		return Result{Candidates: []component.Candidate{}, Diagnostics: diagnostics}, nil
	}

	type aggregate struct {
		candidate component.Candidate
		score     float64
	}
	merged := make(map[string]*aggregate)
	for i, result := range results {
		if result.err != nil {
			continue
		}
		native := make([]float64, len(result.candidates))
		for j, candidate := range result.candidates {
			if err := candidate.Validate(); err != nil {
				diagnostics = append(diagnostics, Diagnostic{Lane: fusion.lanes[i].Name, Err: err})
				native = nil
				break
			}
			native[j] = candidate.Score
		}
		if native == nil {
			successWeight -= fusion.lanes[i].Weight
			continue
		}
		calibrated, err := fusion.lanes[i].Calibrator.Calibrate(CalibrationInput{Query: request.Query, Scores: native})
		if err != nil || len(calibrated) != len(result.candidates) {
			if err == nil {
				err = errors.New("calibrator returned wrong score count")
			}
			diagnostics = append(diagnostics, Diagnostic{Lane: fusion.lanes[i].Name, Err: err})
			successWeight -= fusion.lanes[i].Weight
			continue
		}
		for j := range result.candidates {
			if calibrated[j] < 0 || calibrated[j] > 1 || math.IsNaN(calibrated[j]) || math.IsInf(calibrated[j], 0) {
				diagnostics = append(diagnostics, Diagnostic{Lane: fusion.lanes[i].Name, Err: errors.New("calibrator returned score outside [0,1]")})
				successWeight -= fusion.lanes[i].Weight
				calibrated = nil
				break
			}
		}
		if calibrated == nil {
			continue
		}
		weight := fusion.lanes[i].Weight
		laneCandidates := make(map[string]int, len(result.candidates))
		for j, candidate := range result.candidates {
			key := stableKey(candidate)
			if previous, exists := laneCandidates[key]; exists {
				if calibrated[j] > calibrated[previous] {
					laneCandidates[key] = j
				}
				continue
			}
			laneCandidates[key] = j
		}
		keys := make([]string, 0, len(laneCandidates))
		for key := range laneCandidates {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			j := laneCandidates[key]
			candidate := result.candidates[j]
			item := merged[key]
			if item == nil {
				clone := candidate.Clone()
				clone.Lane = "fusion"
				clone.Score = 0
				item = &aggregate{candidate: clone}
				merged[key] = item
			}
			item.score += weight * calibrated[j]
			item.candidate.Explanation.Terms = append(item.candidate.Explanation.Terms, component.ScoreTerm{
				Lane: fusion.lanes[i].Name, Raw: candidate.Score, Calibrated: calibrated[j],
				Weight: weight, CalibrationVersion: fusion.lanes[i].Calibrator.Version(),
			})
		}
	}
	if successWeight <= 0 {
		return Result{Candidates: []component.Candidate{}, Diagnostics: diagnostics}, nil
	}
	candidates := make([]component.Candidate, 0, len(merged))
	for _, item := range merged {
		item.candidate.Score = item.score / successWeight
		for index := range item.candidate.Explanation.Terms {
			term := &item.candidate.Explanation.Terms[index]
			term.Weight /= successWeight
			term.Contribution = term.Weight * term.Calibrated
		}
		sort.Slice(item.candidate.Explanation.Terms, func(i, j int) bool {
			return item.candidate.Explanation.Terms[i].Lane < item.candidate.Explanation.Terms[j].Lane
		})
		candidates = append(candidates, item.candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			return stableKey(candidates[i]) < stableKey(candidates[j])
		}
		return candidates[i].Score > candidates[j].Score
	})
	if request.Limit > 0 && len(candidates) > request.Limit {
		candidates = candidates[:request.Limit]
	}
	return Result{Candidates: candidates, Diagnostics: diagnostics}, nil
}

func stableKey(candidate component.Candidate) string {
	address := candidate.Address
	if !address.IsZero() {
		return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", address.Kind, address.ConversationID, address.DatasetID, address.DocumentID, address.ItemID)
	}
	return candidate.ID
}

func finiteScores(scores []float64) error {
	for _, score := range scores {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return errors.New("fusion: native score must be finite")
		}
	}
	return nil
}
