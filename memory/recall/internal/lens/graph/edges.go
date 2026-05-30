package graph

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
	"github.com/GizClaw/flowcraft/memory/text/normalize"
)

// Defaults bound deterministic edge generation (docs §8.4).
const (
	DefaultMaxHops                     = 2
	defaultMaxEdgesPerFact             = 10
	defaultMaxCooccurrenceParticipants = 6
	defaultMinConfidence               = 0.0
)

// Config tunes edge extraction. Zero values use the defaults above.
type Config struct {
	MaxEdgesPerFact             int
	MaxCooccurrenceParticipants int
	MinConfidence               float64
}

func (c Config) maxEdgesPerFact() int {
	if c.MaxEdgesPerFact > 0 {
		return c.MaxEdgesPerFact
	}
	return defaultMaxEdgesPerFact
}

func (c Config) maxCooccurrenceParticipants() int {
	if c.MaxCooccurrenceParticipants > 0 {
		return c.MaxCooccurrenceParticipants
	}
	return defaultMaxCooccurrenceParticipants
}

func (c Config) minConfidence() float64 {
	if c.MinConfidence > 0 {
		return c.MinConfidence
	}
	return defaultMinConfidence
}

type directedEdge struct {
	from       string
	to         string
	predicate  string
	factID     string
	agentID    string
	cooccurred bool
}

// extractEdges builds the deterministic edge set for one fact.
func extractEdges(f domain.TemporalFact, cfg Config, now time.Time) []directedEdge {
	if f.ID == "" || !domain.IsHistorical(f, now) {
		return nil
	}
	if f.Confidence < cfg.minConfidence() {
		return nil
	}
	typed := extractRelationEdges(f)
	switch f.Kind {
	case domain.KindRelation:
		return typed
	case domain.KindEvent, domain.KindState, domain.KindProcedure, domain.KindNote:
		return append(typed, extractCooccurrenceEdges(f, cfg)...)
	default:
		return typed
	}
}

func extractRelationEdges(f domain.TemporalFact) []directedEdge {
	sub := canonicalNode(f.Subject)
	pred := strings.TrimSpace(f.Predicate)
	obj := canonicalNode(f.Object)
	if sub == "" || pred == "" || obj == "" || sub == obj {
		return nil
	}
	if isCommonNoun(sub) || isCommonNoun(obj) {
		return nil
	}
	return []directedEdge{{
		from: sub, to: obj, predicate: pred, factID: f.ID, agentID: f.Scope.AgentID,
	}}
}

func extractCooccurrenceEdges(f domain.TemporalFact, cfg Config) []directedEdge {
	nodes := collectCooccurrenceNodes(f, cfg.maxCooccurrenceParticipants())
	if len(nodes) < 2 {
		return nil
	}
	maxEdges := cfg.maxEdgesPerFact()
	var out []directedEdge
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			if len(out) >= maxEdges {
				return out
			}
			out = append(out,
				directedEdge{from: nodes[i], to: nodes[j], factID: f.ID, agentID: f.Scope.AgentID, cooccurred: true},
				directedEdge{from: nodes[j], to: nodes[i], factID: f.ID, agentID: f.Scope.AgentID, cooccurred: true},
			)
		}
	}
	return out
}

func collectCooccurrenceNodes(f domain.TemporalFact, limit int) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(s string) {
		n := canonicalNode(s)
		if n == "" || isCommonNoun(n) {
			return
		}
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	for _, e := range f.Entities {
		add(e)
		if len(out) >= limit {
			return out
		}
	}
	for _, p := range f.Participants {
		add(p)
		if len(out) >= limit {
			return out
		}
	}
	add(f.Subject)
	if len(out) >= limit {
		return out
	}
	add(f.Object)
	return out
}

// canonicalNode matches entity projection mention normalization.
func canonicalNode(s string) string {
	return strings.ToLower(normalize.CollapseSpaces(s))
}

func isCommonNoun(node string) bool {
	return words.IsCommonGraphNoun(node)
}
