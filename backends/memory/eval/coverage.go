package eval

import (
	"context"
	"strings"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TurnCoverage says whether a pack stands for one dataset turn, and how.
type TurnCoverage struct {
	// Rank is the covering item's position in the pack, or -1 when no item
	// stands for the turn.
	Rank int
	// ViaRaw is true when the turn's own wording reached the pack: an item that
	// is the turn's message, or one carrying its committed text. A false value
	// with a covered turn means a derived item (a fact, a summary) stood for it,
	// so the model read a paraphrase rather than the dataset's own wording.
	ViaRaw bool
}

// Covered reports whether the pack stands for the turn.
func (coverage TurnCoverage) Covered() bool { return coverage.Rank >= 0 }

// Matcher answers the one question grading, the retrieval probes and the
// diagnostic all ask of a pack: does it stand for this dataset turn, and did the
// turn's own wording reach it? One implementation keeps a report and a diagnosis
// from disagreeing about what recall means, and resolves each item's provenance
// once per question instead of once per evidence turn.
//
// A Matcher is not safe for concurrent use: build one per question.
type Matcher struct {
	resolver ProvenanceResolver
	sources  map[string][]ResolvedSource
}

// NewMatcher returns a matcher over a resolver. A nil resolver leaves coverage
// on the item contents alone, which is what a host that wires no provenance
// gets.
func NewMatcher(resolver ProvenanceResolver) *Matcher {
	return &Matcher{resolver: resolver, sources: map[string][]ResolvedSource{}}
}

// Sources returns the canonical messages an item stands on, memoized per item.
func (matcher *Matcher) Sources(ctx context.Context, item corememory.ContextItem) []ResolvedSource {
	if matcher == nil || matcher.resolver == nil {
		return nil
	}
	sources, cached := matcher.sources[item.ID]
	if !cached {
		sources = matcher.resolver.ResolveSources(ctx, item)
		matcher.sources[item.ID] = sources
	}
	return sources
}

// Cover reports where items first stand for the dataset turn: prefer an item
// carrying the turn's own committed text over one whose provenance merely
// resolves to it, so a pack that holds both is reported as a raw hit.
//
// Two comparisons decide it. The first is the committed text, which is all the
// harness can do for a store that carries no dataset turn ids and all it can do
// for a dataset that names no turns. The second is turn identity: a source
// message whose ingest-side turn id is turnID. It is the one that survives a
// rendering difference between the loader and the store -- the loader puts an
// image caption in the turn's text, while a store written with "-images native"
// attaches the image as a part instead -- and it is what lets a turn be counted
// when a store written before ingest tagged turns cannot match by text at all.
func (matcher *Matcher) Cover(ctx context.Context, items []corememory.ContextItem, turnID, turnText string) TurnCoverage {
	missing := TurnCoverage{Rank: -1}
	for rank, item := range items {
		if turnText != "" && strings.Contains(item.Content.Text(), turnText) {
			return TurnCoverage{Rank: rank, ViaRaw: true}
		}
		for _, source := range matcher.Sources(ctx, item) {
			if turnText != "" && strings.Contains(source.Text, turnText) {
				return TurnCoverage{Rank: rank}
			}
		}
	}
	if matcher == nil || matcher.resolver == nil || turnID == "" {
		return missing
	}
	for rank, item := range items {
		for _, source := range matcher.Sources(ctx, item) {
			if source.TurnID != turnID {
				continue
			}
			// An item that *is* the turn's message carries the wording the
			// dataset graded, whatever the loader's rendering of it.
			return TurnCoverage{Rank: rank, ViaRaw: item.Kind == corememory.ContextRawMessage}
		}
	}
	return missing
}
