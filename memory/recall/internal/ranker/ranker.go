package ranker

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/evolution"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

const (
	defaultHalfLife         = 0
	defaultSupersededFactor = 0.5
)

// Default is the deterministic post-assessment ranker.
type Default struct {
	halfLife         time.Duration
	supersededFactor float64
}

// NewDefault constructs a ranker with v1-aligned decay defaults.
func NewDefault(opts ...Option) *Default {
	d := &Default{
		halfLife:         defaultHalfLife,
		supersededFactor: defaultSupersededFactor,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

var _ port.Ranker = (*Default)(nil)

// Rank implements port.Ranker.
func (d *Default) Rank(_ context.Context, in port.RankInput) port.RankOutput {
	if len(in.Items) == 0 {
		return port.RankOutput{}
	}
	type scoredItem struct {
		item  domain.ContextItem
		score float64
	}
	items := make([]scoredItem, 0, len(in.Items))
	for i, item := range in.Items {
		items = append(items, scoredItem{item: item, score: rankInputAssessmentScore(in, i)})
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	var boosts, timeDecay, superseded int
	for i := range items {
		boost := factRankBoost(items[i].item)
		if boost != 0 {
			boosts++
			items[i].score += boost
			if items[i].item.Candidate.Metadata == nil {
				items[i].item.Candidate.Metadata = map[string]any{}
			}
			items[i].item.Candidate.Metadata["rank_boost"] = boost
		}
		if d.halfLife > 0 {
			if decay := d.timeDecayFactor(items[i].item.Fact, now); decay < 1 {
				items[i].score *= decay
				timeDecay++
				if items[i].item.Candidate.Metadata == nil {
					items[i].item.Candidate.Metadata = map[string]any{}
				}
				items[i].item.Candidate.Metadata["time_decay"] = decay
			}
		}
		if domain.IsSuperseded(items[i].item.Fact) && d.supersededFactor > 0 && d.supersededFactor < 1 {
			items[i].score *= d.supersededFactor
			superseded++
			if items[i].item.Candidate.Metadata == nil {
				items[i].item.Candidate.Metadata = map[string]any{}
			}
			items[i].item.Candidate.Metadata["superseded_decay"] = d.supersededFactor
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].score > items[j].score
	})
	if in.FinalCap > 0 && len(items) > in.FinalCap {
		items = items[:in.FinalCap]
	}
	outItems := make([]domain.ContextItem, 0, len(items))
	outScores := make([]float64, 0, len(items))
	for _, item := range items {
		outItems = append(outItems, item.item)
		outScores = append(outScores, item.score)
	}
	return port.RankOutput{
		Items:                  outItems,
		RankScores:             outScores,
		BoostsApplied:          boosts,
		TimeDecayApplied:       timeDecay,
		SupersededDecayApplied: superseded,
	}
}

func rankInputAssessmentScore(in port.RankInput, i int) float64 {
	if i >= 0 && i < len(in.AssessmentScores) {
		return in.AssessmentScores[i]
	}
	return 0
}

func (d *Default) timeDecayFactor(f domain.TemporalFact, now time.Time) float64 {
	ts := domain.EffectiveTimestamp(f)
	if ts.IsZero() {
		return 1
	}
	age := now.Sub(ts).Seconds()
	if age < 0 {
		age = 0
	}
	return math.Exp(-math.Ln2 * age / d.halfLife.Seconds())
}

func factRankBoost(item domain.ContextItem) float64 {
	f := item.Fact
	var boost float64
	if f.Confidence > 0 {
		c := f.Confidence
		if c > 1 {
			c = 1
		}
		boost += c * 0.004
	}
	boost += (evolution.FeedbackBoost(f.Reinforcement, f.Penalty) - 1) * 0.02
	return boost
}

// Option configures a Default ranker.
type Option func(*Default)

// WithTimeDecay sets the half-life for opt-in recency decay. The default
// disables wall-clock decay because long-term recall should not penalize
// historically correct evidence unless the caller explicitly asks for recency.
func WithTimeDecay(halfLife time.Duration) Option {
	return func(d *Default) { d.halfLife = halfLife }
}

// WithSupersededDecay sets the score multiplier for superseded facts (default 0.5).
func WithSupersededDecay(factor float64) Option {
	return func(d *Default) { d.supersededFactor = factor }
}
