// Package lifecycle implements durable, replayable memory lifecycle phases.
package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

const (
	DecayAlgorithmVersion  = "retention-v2"
	RepairAlgorithmVersion = "repair-v1"
)

type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type DecayConfig struct {
	Version         string        `json:"version"`
	HalfLife        time.Duration `json:"half_life"`
	RecencyWeight   float64       `json:"recency_weight"`
	FrequencyWeight float64       `json:"frequency_weight"`
	RelevanceWeight float64       `json:"relevance_weight"`
	FrequencyScale  float64       `json:"frequency_scale"`
}

type DecayInput struct {
	// BaseTime is the immutable fact/observation event time used before the
	// first recall. LastAccessAt supersedes it once recall evidence exists.
	BaseTime     time.Time
	LastAccessAt time.Time
	AccessCount  uint64
	Relevance    float64
}

type ScoreSnapshot struct {
	ObservationID    string                `json:"observation_id"`
	Score            float64               `json:"score"`
	Recency          float64               `json:"recency"`
	Frequency        float64               `json:"frequency"`
	Relevance        float64               `json:"relevance"`
	ScoredAt         time.Time             `json:"scored_at"`
	AlgorithmVersion string                `json:"algorithm_version"`
	SourceRefs       []sdkmemory.SourceRef `json:"source_refs,omitempty"`
}

type Decay struct {
	config DecayConfig
	clock  Clock
}

func NewDecay(config DecayConfig, clock Clock) (*Decay, error) {
	if config.Version == "" {
		config.Version = DecayAlgorithmVersion
	}
	if config.FrequencyScale == 0 {
		config.FrequencyScale = 10
	}
	if config.Version != DecayAlgorithmVersion || config.HalfLife <= 0 ||
		!finiteNonNegative(config.RecencyWeight) || !finiteNonNegative(config.FrequencyWeight) ||
		!finiteNonNegative(config.RelevanceWeight) || !finitePositive(config.FrequencyScale) {
		return nil, errors.New("memory lifecycle: invalid decay config")
	}
	if config.RecencyWeight+config.FrequencyWeight+config.RelevanceWeight <= 0 {
		return nil, errors.New("memory lifecycle: decay weights must have a positive sum")
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &Decay{config: config, clock: clock}, nil
}

// Score computes:
// retention = wr*2^(-age/halfLife) + wf*min(1,log1p(count)/log1p(scale))
//   - wv*clamp(relevance,0,1), divided by the weight sum.
//
// A zero LastAccessAt falls back to BaseTime. A zero/future effective time has
// age zero. All timestamps are compared in UTC.
func (decay *Decay) Score(input DecayInput) ScoreSnapshot {
	now := decay.clock.Now().UTC()
	last := input.LastAccessAt.UTC()
	if last.IsZero() {
		last = input.BaseTime.UTC()
	}
	age := time.Duration(0)
	if !last.IsZero() && last.Before(now) {
		age = now.Sub(last)
	}
	recency := math.Pow(2, -float64(age)/float64(decay.config.HalfLife))
	frequency := math.Log1p(float64(input.AccessCount)) / math.Log1p(decay.config.FrequencyScale)
	frequency = clamp(frequency)
	relevance := clamp(input.Relevance)
	weights := decay.config.RecencyWeight + decay.config.FrequencyWeight + decay.config.RelevanceWeight
	score := (decay.config.RecencyWeight*recency + decay.config.FrequencyWeight*frequency +
		decay.config.RelevanceWeight*relevance) / weights
	return ScoreSnapshot{Score: clamp(score), Recency: recency, Frequency: frequency, Relevance: relevance,
		ScoredAt: now, AlgorithmVersion: decay.config.Version}
}

type ForgetMode string

const (
	ModeAuditOnly      ForgetMode = "audit_only"
	ModeSoftVisibility ForgetMode = "soft_visibility"
)

type ForgetConfig struct {
	Mode                 ForgetMode `json:"mode"`
	EnableSoftVisibility bool       `json:"enable_soft_visibility"`
	SoftForgetThreshold  float64    `json:"soft_forget_threshold"`
	ArchiveThreshold     float64    `json:"archive_threshold"`
}

type ForgetCandidate struct {
	ObservationID string                `json:"observation_id"`
	Disposition   string                `json:"disposition"`
	Reason        string                `json:"reason"`
	Score         float64               `json:"score"`
	Threshold     float64               `json:"threshold"`
	SourceRefs    []sdkmemory.SourceRef `json:"source_refs,omitempty"`
	Apply         bool                  `json:"apply"`
}

type ForgetPlan struct {
	ID               string            `json:"id"`
	Mode             ForgetMode        `json:"mode"`
	Candidates       []ForgetCandidate `json:"candidates"`
	CreatedAt        time.Time         `json:"created_at"`
	AlgorithmVersion string            `json:"algorithm_version"`
}

func PlanForget(config ForgetConfig, snapshots []ScoreSnapshot, now time.Time) ForgetPlan {
	if config.Mode == "" {
		config.Mode = ModeAuditOnly
	}
	values := append([]ScoreSnapshot(nil), snapshots...)
	sort.Slice(values, func(i, j int) bool { return values[i].ObservationID < values[j].ObservationID })
	candidates := make([]ForgetCandidate, 0)
	for _, snapshot := range values {
		disposition, threshold := "", 0.0
		if config.ArchiveThreshold > 0 && snapshot.Score <= config.ArchiveThreshold {
			disposition, threshold = "archive_candidate", config.ArchiveThreshold
		} else if config.SoftForgetThreshold > 0 && snapshot.Score <= config.SoftForgetThreshold {
			disposition, threshold = "soft_forget_candidate", config.SoftForgetThreshold
		}
		if disposition == "" {
			continue
		}
		apply := config.Mode == ModeSoftVisibility && config.EnableSoftVisibility &&
			disposition == "soft_forget_candidate" && threshold > 0 && threshold <= 1
		candidates = append(candidates, ForgetCandidate{
			ObservationID: snapshot.ObservationID, Disposition: disposition,
			Reason: "retention score at or below configured threshold", Score: snapshot.Score,
			Threshold: threshold, SourceRefs: append([]sdkmemory.SourceRef(nil), snapshot.SourceRefs...), Apply: apply,
		})
	}
	payload, _ := json.Marshal(struct {
		Mode       ForgetMode        `json:"mode"`
		Candidates []ForgetCandidate `json:"candidates"`
		Version    string            `json:"version"`
	}{config.Mode, candidates, DecayAlgorithmVersion})
	return ForgetPlan{ID: digest("forget-plan", payload), Mode: config.Mode, Candidates: candidates,
		CreatedAt: now.UTC(), AlgorithmVersion: DecayAlgorithmVersion}
}

type RepairActionKind string

const (
	ActionReplay     RepairActionKind = "replay"
	ActionQuarantine RepairActionKind = "quarantine"
	ActionRebuild    RepairActionKind = "rebuild"
)

type RepairAction struct {
	Kind     RepairActionKind `json:"kind"`
	Target   string           `json:"target"`
	Evidence string           `json:"evidence"`
}
type RepairPlan struct {
	ID               string          `json:"id"`
	Scope            sdkmemory.Scope `json:"scope"`
	Actions          []RepairAction  `json:"actions"`
	AlgorithmVersion string          `json:"algorithm_version"`
}
type FactEvidence struct {
	ID        string
	LinkedIDs []string
}
type ObservationEvidence struct {
	ID       string
	Replaces string
}

type SummaryInputKind string

const (
	SummaryInputFact    SummaryInputKind = "fact"
	SummaryInputSummary SummaryInputKind = "summary"
)

type SummaryEvidence struct {
	ID                                 string
	Level                              uint8
	InputKind                          SummaryInputKind
	InputIDs                           []string
	CoverageValid                      bool
	SourceDigest, ComputedSourceDigest string
}
type SourceViewEvidence struct{ Name, SourceDigest, ViewDigest string }
type ProjectionEvidence struct {
	Name                                     string
	StoredSourceDigest, ComputedSourceDigest string
	StoredBuildDigest, ComputedBuildDigest   string
}
type RepairInput struct {
	Facts        []FactEvidence
	Observations []ObservationEvidence
	Summaries    []SummaryEvidence
	Sources      []SourceViewEvidence
	Projections  []ProjectionEvidence
}

func InspectRepair(scope sdkmemory.Scope, input RepairInput) RepairPlan {
	plan, _ := InspectRepairContext(context.Background(), scope, input)
	return plan
}

func InspectRepairContext(ctx context.Context, scope sdkmemory.Scope, input RepairInput) (RepairPlan, error) {
	if err := scope.Validate(); err != nil {
		return RepairPlan{}, err
	}
	factIDs := map[string]struct{}{}
	for _, value := range input.Facts {
		factIDs[value.ID] = struct{}{}
	}
	var actions []RepairAction
	for _, value := range input.Facts {
		for _, linked := range value.LinkedIDs {
			if _, exists := factIDs[linked]; !exists {
				actions = append(actions, RepairAction{ActionReplay, "fact:" + value.ID, "dangling linked id:" + linked})
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return RepairPlan{}, err
	}
	if err := validateObservationEvidence(input.Observations); err != nil {
		actions = append(actions, RepairAction{ActionQuarantine, "observations", err.Error()})
	}
	summaryIDs := make(map[string]struct{}, len(input.Summaries))
	for _, value := range input.Summaries {
		summaryIDs[value.ID] = struct{}{}
	}
	for _, value := range input.Summaries {
		invalidInputs := len(value.InputIDs) == 0 || value.Level > 3
		expectedKind := SummaryInputSummary
		if value.Level == 0 {
			expectedKind = SummaryInputFact
		}
		if value.InputKind != expectedKind {
			invalidInputs = true
		}
		for _, id := range value.InputIDs {
			if id == value.ID {
				invalidInputs = true
				break
			}
			var exists bool
			if value.Level == 0 {
				_, exists = factIDs[id]
			} else {
				_, exists = summaryIDs[id]
			}
			if !exists {
				invalidInputs = true
				break
			}
		}
		if invalidInputs || !value.CoverageValid || value.SourceDigest != value.ComputedSourceDigest {
			actions = append(actions, RepairAction{ActionRebuild, "summary:" + value.ID, "input/coverage/source digest mismatch"})
		}
	}
	for _, value := range input.Sources {
		if value.SourceDigest != value.ViewDigest {
			actions = append(actions, RepairAction{ActionReplay, "view:" + value.Name, "source/view digest mismatch"})
		}
	}
	for _, value := range input.Projections {
		if value.StoredBuildDigest != value.ComputedBuildDigest ||
			value.StoredSourceDigest != value.ComputedSourceDigest {
			actions = append(actions, RepairAction{ActionRebuild, "projection:" + value.Name, "build/source digest mismatch"})
		}
	}
	sort.Slice(actions, func(i, j int) bool {
		if actions[i].Target != actions[j].Target {
			return actions[i].Target < actions[j].Target
		}
		return actions[i].Evidence < actions[j].Evidence
	})
	payload, _ := json.Marshal(struct {
		Scope   sdkmemory.Scope
		Actions []RepairAction
		Version string
	}{scope, actions, RepairAlgorithmVersion})
	return RepairPlan{ID: digest("repair-plan", payload), Scope: scope, Actions: actions, AlgorithmVersion: RepairAlgorithmVersion}, nil
}

func validateObservationEvidence(values []ObservationEvidence) error {
	parent := map[string]string{}
	for _, value := range values {
		if value.ID == value.Replaces && value.ID != "" {
			return errors.New("observation self replacement")
		}
		parent[value.ID] = value.Replaces
	}
	for id := range parent {
		seen := map[string]struct{}{}
		for current := id; current != ""; current = parent[current] {
			if _, exists := seen[current]; exists {
				return errors.New("observation replacement cycle")
			}
			seen[current] = struct{}{}
		}
	}
	return nil
}

func clamp(value float64) float64 {
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 1 || math.IsInf(value, 1) {
		return 1
	}
	return value
}
func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
func finiteNonNegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
func digest(domain string, value []byte) string {
	sum := sha256.Sum256(append([]byte("flowcraft.memory."+domain+"\x00v1\x00"), value...))
	return hex.EncodeToString(sum[:])
}
