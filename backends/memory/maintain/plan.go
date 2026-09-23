// Package maintain implements host-invoked memory maintenance: it detects
// superseded facts (soft merge) and stale facts (decay) and records a
// read-path overlay, keeping the canonical fact text immutable and ADD-only.
package maintain

import (
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/internal/textutil"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const AlgorithmVersion = "maintain-v1"

// Config tunes one maintenance pass.
type Config struct {
	// SimilarityThreshold is the token Jaccard similarity two facts about the
	// same entity need before the older one counts as superseded (default 0.5).
	SimilarityThreshold float64
	// SupersedeFactor multiplies the retrieval score of a superseded fact
	// (default 0.5).
	SupersedeFactor float64
	// DecayHalfLife halves a fact's retrieval score every half-life
	// (default 720h).
	DecayHalfLife time.Duration
	// DecayFloor is the score below which an aged fact starts being decayed;
	// a fresh fact scores 1 (default 0.5).
	DecayFloor float64
}

func (config Config) withDefaults() Config {
	if config.SimilarityThreshold == 0 {
		config.SimilarityThreshold = 0.5
	}
	if config.SupersedeFactor == 0 {
		config.SupersedeFactor = 0.5
	}
	if config.DecayHalfLife == 0 {
		config.DecayHalfLife = 720 * time.Hour
	}
	if config.DecayFloor == 0 {
		config.DecayFloor = 0.5
	}
	return config
}

func (config Config) validate() error {
	if !finite(config.SimilarityThreshold) || config.SimilarityThreshold < 0 || config.SimilarityThreshold > 1 {
		return errors.New("maintain: similarity threshold must be in [0,1]")
	}
	if !finite(config.SupersedeFactor) || config.SupersedeFactor <= 0 || config.SupersedeFactor >= 1 {
		return errors.New("maintain: supersede factor must be in (0,1)")
	}
	if config.DecayHalfLife <= 0 {
		return errors.New("maintain: decay half-life must be positive")
	}
	if !finite(config.DecayFloor) || config.DecayFloor <= 0 || config.DecayFloor > 1 {
		return errors.New("maintain: decay floor must be in (0,1]")
	}
	return nil
}

// Supersede marks one older fact as superseded by a newer one.
type Supersede struct {
	FactID       string  `json:"fact_id"`
	Conversation string  `json:"conversation_id"`
	SupersededBy string  `json:"superseded_by"`
	Similarity   float64 `json:"similarity"`
}

// Decay marks one aged fact with its retention score.
type Decay struct {
	FactID       string    `json:"fact_id"`
	Conversation string    `json:"conversation_id"`
	Score        float64   `json:"score"`
	EventTime    time.Time `json:"event_time"`
}

// Plan is the deterministic output of one maintenance pass. It is pure data:
// applying it writes a read-path overlay and never edits canonical facts.
type Plan struct {
	Scope            corememory.Scope `json:"scope"`
	Supersedes       []Supersede      `json:"supersedes,omitempty"`
	Decays           []Decay          `json:"decays,omitempty"`
	AlgorithmVersion string           `json:"algorithm_version"`
}

// Detect plans one scope's soft merges and decay from its conversations.
// conversations maps a conversation ID to its facts.
func Detect(
	scope corememory.Scope,
	conversations map[string][]factview.Fact,
	now time.Time,
) (Plan, error) {
	return DetectWithConfig(scope, conversations, now, Config{})
}

// DetectWithConfig is Detect with explicit tuning.
func DetectWithConfig(
	scope corememory.Scope,
	conversations map[string][]factview.Fact,
	now time.Time,
	config Config,
) (Plan, error) {
	if err := scope.Validate(); err != nil {
		return Plan{}, err
	}
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return Plan{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	plan := Plan{Scope: scope, AlgorithmVersion: AlgorithmVersion}
	ids := make([]string, 0, len(conversations))
	for conversation := range conversations {
		ids = append(ids, conversation)
	}
	sort.Strings(ids)
	for _, conversation := range ids {
		facts := append([]factview.Fact(nil), conversations[conversation]...)
		sort.SliceStable(facts, func(i, j int) bool {
			left, right := recencyKey(facts[i]), recencyKey(facts[j])
			if left.Equal(right) {
				return facts[i].ID < facts[j].ID
			}
			return left.Before(right)
		})
		for index, older := range facts {
			if decay := decayScore(older, now, config); decay < config.DecayFloor {
				plan.Decays = append(plan.Decays, Decay{
					FactID: older.ID, Conversation: conversation, Score: decay, EventTime: recencyKey(older),
				})
			}
			if len(older.Entities) == 0 {
				continue
			}
			for newerIndex := index + 1; newerIndex < len(facts); newerIndex++ {
				newer := facts[newerIndex]
				if len(newer.Entities) == 0 || !sharesEntity(older.Entities, newer.Entities) {
					continue
				}
				similarity := textSimilarity(older.Text, newer.Text)
				if similarity < config.SimilarityThreshold {
					continue
				}
				plan.Supersedes = append(plan.Supersedes, Supersede{
					FactID: older.ID, Conversation: conversation,
					SupersededBy: newer.ID, Similarity: similarity,
				})
				break
			}
		}
	}
	sort.Slice(plan.Supersedes, func(i, j int) bool { return plan.Supersedes[i].FactID < plan.Supersedes[j].FactID })
	sort.Slice(plan.Decays, func(i, j int) bool { return plan.Decays[i].FactID < plan.Decays[j].FactID })
	return plan, nil
}

// recencyKey orders facts by event time, falling back to creation time so a
// fact without an explicit event still participates in the ordering.
func recencyKey(fact factview.Fact) time.Time {
	if !fact.EventTime.IsZero() {
		return fact.EventTime.UTC()
	}
	return fact.CreatedAt.UTC()
}

func decayScore(fact factview.Fact, now time.Time, config Config) float64 {
	age := now.Sub(recencyKey(fact))
	if age <= 0 {
		return 1
	}
	score := math.Pow(2, -float64(age)/float64(config.DecayHalfLife))
	if score < 0.01 {
		score = 0.01
	}
	return score
}

func sharesEntity(left, right []string) bool {
	owned := make(map[string]struct{}, len(left))
	for _, entity := range left {
		owned[normalizeEntity(entity)] = struct{}{}
	}
	for _, entity := range right {
		if _, ok := owned[normalizeEntity(entity)]; ok {
			return true
		}
	}
	return false
}

func normalizeEntity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// textSimilarity is token Jaccard similarity over the two facts.
func textSimilarity(left, right string) float64 {
	leftTokens := tokenSet(left)
	rightTokens := tokenSet(right)
	if len(leftTokens) == 0 || len(rightTokens) == 0 {
		return 0
	}
	shared := 0
	for token := range leftTokens {
		if _, ok := rightTokens[token]; ok {
			shared++
		}
	}
	union := len(leftTokens) + len(rightTokens) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

func tokenSet(value string) map[string]struct{} {
	tokens := textutil.Tokens(value)
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		set[token] = struct{}{}
	}
	return set
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
