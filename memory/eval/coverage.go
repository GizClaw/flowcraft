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

// Cover reports where items best stand for the dataset turn, and whether the
// turn's own wording reached the pack.
//
// An item stands for the turn in one of two ways that do not score alike: it
// carries the graded wording, or it merely stands on the turn. Only the first
// puts that wording in the prompt, so it wins at any rank. Comparing per item
// instead let a high-ranked fact shadow the turn's own message lower in the
// pack: on the 60-question LoCoMo run this was measured against, 23 of the 60
// evidence turns labelled as paraphrases had their message packed too, and the
// label said "the model read a paraphrase" about a prompt that held the text.
//
// Three comparisons decide which of the two it is, and the last two call an
// item a paraphrase. The item's own content may carry the wording (the turn's
// message, or a derived item a source quote was folded into). Its canonical
// sources may carry the wording, which is all the harness can do for a store
// that holds no dataset turn ids and all it can do for a dataset that names no
// turns. Its canonical sources may carry the turn id ingest tagged the message
// with, which is the comparison that survives a rendering difference between
// the loader and the store -- the loader puts an image caption in the turn's
// text, while a store written with "-images native" attaches the image as a
// part instead -- and the one that counts a turn at all when a store written
// before ingest tagged turns has no text left to match.
func (matcher *Matcher) Cover(ctx context.Context, items []corememory.ContextItem, turnID, turnText string) TurnCoverage {
	if turnID == "" && turnText == "" {
		return TurnCoverage{Rank: -1}
	}
	// What a paraphrase can reach is the lowest-ranked item standing on the
	// turn, and it is the answer only when no item carries its wording.
	derived := TurnCoverage{Rank: -1}
	for rank, item := range items {
		sources := matcher.Sources(ctx, item)
		if carriesTurnWording(item, sources, turnID, turnText) {
			return TurnCoverage{Rank: rank, ViaRaw: true}
		}
		if !derived.Covered() && standsOnTurn(sources, turnID, turnText) {
			derived = TurnCoverage{Rank: rank}
		}
	}
	return derived
}

// carriesTurnWording reports that this item puts the turn's own words in the
// prompt, whatever its rank.
func carriesTurnWording(item corememory.ContextItem, sources []ResolvedSource, turnID, turnText string) bool {
	if turnText != "" && strings.Contains(item.Content.Text(), turnText) {
		return true
	}
	if item.Kind != corememory.ContextRawMessage {
		return false
	}
	// An item that *is* the turn's message carries the wording the dataset
	// graded, whatever the loader's rendering of that turn says: a caption one
	// side holds and the other does not leaves the two texts unequal while the
	// record stays the turn's own.
	return standsOnTurn(sources, turnID, turnText)
}

// standsOnTurn reports whether the canonical messages an item resolves to are
// the turn: by the ingest-side turn id first, and by committed text for a store
// that carries no ids. An item that only reaches this is a paraphrase -- its
// own content is what the model read, and for a derived item its sources are
// the whole commit it was extracted from, not the turn it paraphrases.
func standsOnTurn(sources []ResolvedSource, turnID, turnText string) bool {
	for _, source := range sources {
		if turnID != "" && source.TurnID == turnID {
			return true
		}
		if turnText != "" && strings.Contains(source.Text, turnText) {
			return true
		}
	}
	return false
}
