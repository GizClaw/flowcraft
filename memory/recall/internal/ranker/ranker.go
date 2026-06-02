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

// Default is the deterministic post-materialize ranker.
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
	items := append([]domain.ContextItem(nil), in.Items...)
	if len(items) == 0 {
		return port.RankOutput{}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	var boosts, timeDecay, superseded int
	for i := range items {
		boost := factRankBoost(items[i])
		if boost != 0 {
			boosts++
			items[i].Candidate.Score += boost
			if items[i].Candidate.Metadata == nil {
				items[i].Candidate.Metadata = map[string]any{}
			}
			items[i].Candidate.Metadata["rank_boost"] = boost
		}
		if d.halfLife > 0 {
			if decay := d.timeDecayFactor(items[i].Fact, now); decay < 1 {
				items[i].Candidate.Score *= decay
				timeDecay++
				if items[i].Candidate.Metadata == nil {
					items[i].Candidate.Metadata = map[string]any{}
				}
				items[i].Candidate.Metadata["time_decay"] = decay
			}
		}
		if domain.IsSuperseded(items[i].Fact) && d.supersededFactor > 0 && d.supersededFactor < 1 {
			items[i].Candidate.Score *= d.supersededFactor
			superseded++
			if items[i].Candidate.Metadata == nil {
				items[i].Candidate.Metadata = map[string]any{}
			}
			items[i].Candidate.Metadata["superseded_decay"] = d.supersededFactor
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Candidate.Score > items[j].Candidate.Score
	})
	if in.FinalCap > 0 && len(items) > in.FinalCap {
		items = items[:in.FinalCap]
	}
	return port.RankOutput{
		Items:                  items,
		BoostsApplied:          boosts,
		TimeDecayApplied:       timeDecay,
		SupersededDecayApplied: superseded,
	}
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
