package domain

import (
	"context"
	"sort"
)

// LineageRelation classifies how a fact relates to the root fact in
// a lineage traversal. The empty string is reserved as a sentinel
// for "unknown" so consumers can pattern-match exhaustively without
// a fallback case.
type LineageRelation string

const (
	// LineageRelationRoot marks the query fact itself (depth 0).
	LineageRelationRoot LineageRelation = "root"
	// LineageRelationSupersede marks facts reached through the
	// CorrectedBy / Supersedes pointers (either direction). It is
	// also the default classification for descendants whose
	// Revision.Kind is RevisionSupersede or unset.
	LineageRelationSupersede LineageRelation = "supersedes"
	// LineageRelationFork marks descendants whose Revision.Kind is
	// RevisionFork — parallel branches that did NOT close the prior.
	LineageRelationFork LineageRelation = "fork_of"
	// LineageRelationContest marks descendants whose Revision.Kind is
	// RevisionContest — challenges anchored on the prior fact.
	LineageRelationContest LineageRelation = "contest_of"
	// LineageRelationMerge marks descendants whose Revision.Kind is
	// "merge". Reserved for the N:1 merge resolver path; tolerated
	// here so the traversal does not silently mis-classify future
	// merge revisions.
	LineageRelationMerge LineageRelation = "merged_from"
)

// FactLineageNode is one node in a fact lineage DAG produced by
// BuildLineage. Depth is the BFS distance from the root fact
// (root = 0). SourceFactID is the fact whose lookup discovered this
// node (empty on the root); it is NOT necessarily the upstream fact
// id stored in the Revision metadata, since lineage can be reached
// from either direction (e.g. via FindSupersededBy).
type FactLineageNode struct {
	Fact         TemporalFact
	Relation     LineageRelation
	SourceFactID string
	Depth        int
}

// LineageLookups carries the store queries BuildLineage needs. The
// facade wires these from port.TemporalStore so the domain layer
// stays port-agnostic (domain cannot import port — see package
// docstring on port). Any nil function is treated as "no edges of
// that kind", which is convenient for tests that only want to
// exercise one branch of the traversal.
type LineageLookups struct {
	Get                  func(ctx context.Context, scope Scope, id string) (TemporalFact, error)
	FindByRevisionSource func(ctx context.Context, scope Scope, sourceID string) ([]TemporalFact, error)
	FindSupersededBy     func(ctx context.Context, scope Scope, sourceID string) ([]TemporalFact, error)
}

// BuildLineage walks the supersede chain and revision DAG outward
// from root and returns every reachable fact exactly once. The
// traversal follows four edge kinds per visited node:
//
//  1. root.Supersedes  → prior facts (Get + Relation=Supersede)
//  2. root.CorrectedBy → successor fact (Get + Relation=Supersede)
//  3. FindByRevisionSource(root.ID) → descendants whose Revision
//     metadata points back at root; classified by Revision.Kind
//     (Fork / Contest / Merge / Supersede, default Supersede).
//  4. FindSupersededBy(root.ID) → ancestor facts CorrectedBy == root
//     that were not already discovered via (1); enqueued as
//     Supersede so the traversal recovers from missing Supersedes
//     pointers on legacy data.
//
// Results are returned in stable order: root first (depth 0), then
// BFS in increasing depth; within a depth, lexicographically by
// FactID so the output is deterministic regardless of map / store
// iteration order.
//
// The visited set is keyed by FactID so cycles in metadata
// (deliberately malformed data) terminate after each node is
// emitted exactly once.
func BuildLineage(ctx context.Context, root TemporalFact, lookups LineageLookups) ([]FactLineageNode, error) {
	if root.ID == "" {
		return nil, nil
	}

	visited := map[string]struct{}{root.ID: {}}
	out := []FactLineageNode{{
		Fact:     root,
		Relation: LineageRelationRoot,
		Depth:    0,
	}}

	type item struct {
		fact  TemporalFact
		depth int
	}
	queue := []item{{fact: root, depth: 0}}

	enqueue := func(f TemporalFact, rel LineageRelation, src string, depth int) {
		if f.ID == "" {
			return
		}
		if _, seen := visited[f.ID]; seen {
			return
		}
		visited[f.ID] = struct{}{}
		out = append(out, FactLineageNode{
			Fact:         f,
			Relation:     rel,
			SourceFactID: src,
			Depth:        depth,
		})
		queue = append(queue, item{fact: f, depth: depth})
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		nextDepth := cur.depth + 1

		for _, prior := range cur.fact.Supersedes {
			if prior == "" {
				continue
			}
			if _, seen := visited[prior]; seen {
				continue
			}
			if lookups.Get == nil {
				continue
			}
			pf, err := lookups.Get(ctx, cur.fact.Scope, prior)
			if err != nil {
				continue
			}
			enqueue(pf, LineageRelationSupersede, cur.fact.ID, nextDepth)
		}

		if cur.fact.CorrectedBy != "" {
			if _, seen := visited[cur.fact.CorrectedBy]; !seen && lookups.Get != nil {
				cf, err := lookups.Get(ctx, cur.fact.Scope, cur.fact.CorrectedBy)
				if err == nil {
					enqueue(cf, LineageRelationSupersede, cur.fact.ID, nextDepth)
				}
			}
		}

		if lookups.FindByRevisionSource != nil {
			descendants, err := lookups.FindByRevisionSource(ctx, cur.fact.Scope, cur.fact.ID)
			if err != nil {
				return nil, err
			}
			for _, d := range descendants {
				if _, seen := visited[d.ID]; seen {
					continue
				}
				enqueue(d, classifyRevision(d), cur.fact.ID, nextDepth)
			}
		}

		if lookups.FindSupersededBy != nil {
			succs, err := lookups.FindSupersededBy(ctx, cur.fact.Scope, cur.fact.ID)
			if err != nil {
				return nil, err
			}
			for _, sf := range succs {
				if _, seen := visited[sf.ID]; seen {
					continue
				}
				enqueue(sf, LineageRelationSupersede, cur.fact.ID, nextDepth)
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		return out[i].Fact.ID < out[j].Fact.ID
	})
	return out, nil
}

// classifyRevision maps a descendant fact's Revision.Kind to the
// LineageRelation surfaced in the DAG. Facts whose Revision is
// missing or RevisionSupersede fall through to Supersede so the
// caller never sees an empty Relation on a non-root node.
func classifyRevision(f TemporalFact) LineageRelation {
	rev, ok := RevisionOf(f)
	if !ok {
		return LineageRelationSupersede
	}
	switch rev.Kind {
	case RevisionFork:
		return LineageRelationFork
	case RevisionContest:
		return LineageRelationContest
	case RevisionKind("merge"):
		return LineageRelationMerge
	default:
		return LineageRelationSupersede
	}
}
