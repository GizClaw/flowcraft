package main

import (
	"context"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// factReader is the slice of the fact store the link walk reads: the record
// behind one fact id. *factview.FactStore implements it; a test stands in
// without a workspace.
type factReader interface {
	Get(ctx context.Context, scope corememory.Scope, conversationID, factID string) (factview.Fact, bool, error)
}

// linkWalk answers the offline half of the link question: with the links the
// write side already stores, how much of a question's missed gold sits within
// one hop of the pool retrieval returned? Retrieval itself is untouched -- the
// walk re-reads what the pool read and follows the record links.
//
// One hop is the whole walk: for every fact in the pool it resolves the
// recorded linked_memory_ids to the records behind them, and a turn counts as
// reached when one of those records stands on it, by the same provenance match
// the rest of this tool grades with. Something two hops out is not reached on
// purpose: the first wiring of this data would add the pool's direct
// neighbors, and the number this walk reports is the ceiling that wiring could
// reach.
//
// Records are memoized across questions, because the same fact is probed many
// times in a run.
type linkWalk struct {
	reader  factReader
	matcher *eval.Matcher
	scope   corememory.Scope

	records map[string]factview.Fact // "conversation/fact" -> record
	missing map[string]bool          // "conversation/fact" -> read, no record
	links   map[string][]string      // "conversation/fact" -> link ids

	// reads counts the distinct fact records looked up; unresolved counts the
	// lookups with no record behind them (a link can outlive the generation
	// that wrote it).
	reads      int
	unresolved int
}

func newLinkWalk(reader factReader, matcher *eval.Matcher, scope corememory.Scope) *linkWalk {
	return &linkWalk{
		reader: reader, matcher: matcher, scope: scope,
		records: map[string]factview.Fact{},
		missing: map[string]bool{},
		links:   map[string][]string{},
	}
}

// neighborhood returns the one-hop targets of a pool: every recorded fact the
// pool's facts link to, as items the matcher can stand a turn on. The pool's
// own items are not part of it -- the question is what is reachable beyond
// them.
func (walk *linkWalk) neighborhood(ctx context.Context, conversationID string, pool []corememory.ContextItem) []corememory.ContextItem {
	if walk == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var targets []corememory.ContextItem
	for _, item := range pool {
		if item.Kind != corememory.ContextFact {
			continue
		}
		for _, linkID := range walk.linksOf(ctx, conversationID, item.ID) {
			key := conversationID + "/" + linkID
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			record, ok := walk.recordOf(ctx, conversationID, linkID)
			if !ok {
				continue
			}
			targets = append(targets, corememory.ContextItem{
				ID: record.ID, Kind: corememory.ContextFact,
				Sources: append([]corememory.SourceRef(nil), record.Provenance...),
			})
		}
	}
	return targets
}

// oneHopFrom returns the pool items that sit one hop from any of the seeds.
//
// Links are read in both directions: the write side stores a link on the fact
// that produced it, so a candidate can either be named by a seed or name one,
// and a boost that only read the forward direction would miss the second shape.
// Seeds and candidates are both pool items, so a hit here is a candidate the
// budget already fetched -- the question this answers is whether the link graph
// could have raised it, not whether retrieval could have found it.
func (walk *linkWalk) oneHopFrom(
	ctx context.Context,
	conversationID string,
	seeds []corememory.ContextItem,
	candidates []corememory.ContextItem,
) map[string]bool {
	set := map[string]bool{}
	if walk == nil {
		return set
	}
	seedIDs := map[string]bool{}
	for _, seed := range seeds {
		if seed.Kind != corememory.ContextFact || seed.ID == "" {
			continue
		}
		seedIDs[seed.ID] = true
		for _, linkID := range walk.linksOf(ctx, conversationID, seed.ID) {
			set[linkID] = true
		}
	}
	for _, candidate := range candidates {
		if candidate.Kind != corememory.ContextFact || candidate.ID == "" {
			continue
		}
		for _, linkID := range walk.linksOf(ctx, conversationID, candidate.ID) {
			if seedIDs[linkID] {
				set[candidate.ID] = true
				break
			}
		}
	}
	return set
}

// linksOf returns the link ids one fact stores, memoized. A pool item with no
// record offers nothing to walk.
func (walk *linkWalk) linksOf(ctx context.Context, conversationID, factID string) []string {
	key := conversationID + "/" + factID
	if links, cached := walk.links[key]; cached {
		return links
	}
	record, ok := walk.recordOf(ctx, conversationID, factID)
	if !ok {
		walk.links[key] = nil
		return nil
	}
	walk.links[key] = append([]string(nil), record.LinkedMemoryIDs...)
	return walk.links[key]
}

// recordOf reads one fact record, memoized, so a fact probed by many questions
// costs one read. A read error is treated like a missing record: the walk
// answers what the store can serve right now, and the summary counts the
// misses.
func (walk *linkWalk) recordOf(ctx context.Context, conversationID, factID string) (factview.Fact, bool) {
	key := conversationID + "/" + factID
	if record, cached := walk.records[key]; cached {
		return record, true
	}
	if walk.missing[key] {
		return factview.Fact{}, false
	}
	walk.reads++
	record, ok, err := walk.reader.Get(ctx, walk.scope, conversationID, factID)
	if err != nil || !ok {
		walk.missing[key] = true
		walk.unresolved++
		return factview.Fact{}, false
	}
	walk.records[key] = record
	return record, true
}
