package recall

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ResolveOp is the action an [UpdateResolver] requests for one
// existing memory in response to a new fact.
type ResolveOp string

const (
	// OpAdd is the default: keep the candidate untouched and accept the
	// new fact as a fresh entry. Resolvers MAY omit ADD actions; the
	// new entry is written regardless.
	OpAdd ResolveOp = "ADD"
	// OpUpdate marks the candidate as superseded by the new fact.
	// FlowCraft tags the candidate with superseded_by=<newID>, NOT
	// overwriting the candidate's content (preserves audit history).
	OpUpdate ResolveOp = "UPDATE"
	// OpDelete marks the candidate as contradicted by the new fact.
	// FlowCraft tags the candidate with superseded_by=<newID> AND
	// tombstone=true so the default Recall filter excludes it. History
	// is still recoverable via Auditable.
	OpDelete ResolveOp = "DELETE"
	// OpNoop performs no action. Use when the new fact equals or is
	// subsumed by the candidate.
	OpNoop ResolveOp = "NOOP"
)

// ResolveAction is one entry of an [UpdateResolver] decision.
//
// SourceID names the new fact (entry ID) that triggered the action.
//
//   - UPDATE / DELETE: SourceID is REQUIRED and MUST be one of the
//     IDs in [ResolveBatch.NewFacts]. FlowCraft drops actions that
//     reference an unknown source so a hallucinated ID cannot mark
//     the wrong entry as superseded. The candidate's MetaSupersededBy
//     is set to SourceID.
//   - ADD / NOOP: SourceID is OPTIONAL (informational only). The new
//     entry is written by the caller regardless of whether ADD is
//     emitted, and NOOP by definition triggers no mutation, so
//     FlowCraft does not read SourceID for these ops. Resolvers MAY
//     leave it empty.
//
// TargetID names the existing memory the action applies to and is
// required for UPDATE and DELETE; it is ignored for ADD and NOOP.
type ResolveAction struct {
	Op       ResolveOp
	SourceID string // new fact entry ID; required for UPDATE/DELETE, optional for ADD/NOOP
	TargetID string // existing memory ID; required for UPDATE/DELETE, ignored for ADD/NOOP
}

// ResolveNewFact pairs an extracted fact with the entry ID FlowCraft
// will write for it. The resolver MUST NOT mutate Fact.
type ResolveNewFact struct {
	EntryID string
	Fact    ExtractedFact
}

// ResolveBatch is the input to [UpdateResolver.Resolve]. All non-slot
// facts produced by a single Save call are passed together so the
// resolver can reason about combined contradictions ("user divorced X"
// + "user married Y" → DELETE the X-spouse memory AND UPDATE other
// X-related memories in one go).
type ResolveBatch struct {
	// NewFacts are the just-extracted facts for which the slot supersede
	// channel did not fire (Subject or Predicate empty). The order is
	// stable across calls so resolver implementations can rely on it
	// when correlating with their own logs.
	NewFacts []ResolveNewFact

	// Candidates is the union of top-K Recall hits across all NewFacts,
	// deduplicated by Doc.ID, ordered by descending Score. The resolver
	// is free to ignore any candidate whose action is ADD (the default).
	Candidates []Hit
}

// UpdateResolver is the contract for opt-in LLM-driven memory
// reconciliation. Implementations receive the full per-Save batch (not
// individual facts) so they can reason about combined updates that no
// single-fact decision could resolve.
//
// The interface intentionally takes a batch even when only one new
// fact qualifies for the resolver path. Single-fact resolvers can
// simply iterate batch.NewFacts and apply per-fact logic.
type UpdateResolver interface {
	Resolve(ctx context.Context, batch ResolveBatch) ([]ResolveAction, error)
}

// runResolverBatch is the single resolver invocation per Save. It
// gathers candidates for every non-slot new fact, unions/deduplicates
// them, calls the resolver once, and applies the returned actions.
// Errors are swallowed (logged) so a flaky resolver never blocks Save.
//
// Cost model: gatherResolverCandidates issues N×Recall (one per new
// fact) before the single LLM call. The span attached here records
// n_facts and n_candidates so dashboards can correlate Save latency
// spikes with resolver fan-out without enabling SearchDebug. Future
// work may expose a custom-candidate hook to let callers replace the
// per-fact Recall with a batched embed + single vector lookup.
func (m *lt) runResolverBatch(
	ctx context.Context, scope Scope,
	newFacts []ResolveNewFact, now time.Time,
) {
	if len(newFacts) == 0 {
		return
	}
	ctx, span := telemetry.Tracer().Start(ctx, "memory.recall.resolver_batch")
	defer span.End()
	t0 := time.Now()
	defer func() {
		resolverDuration.Record(ctx, time.Since(t0).Seconds())
	}()

	candidates := m.gatherResolverCandidates(ctx, scope, newFacts)
	span.SetAttributes(
		attribute.Int("n_facts", len(newFacts)),
		attribute.Int("n_candidates", len(candidates)),
	)
	if len(candidates) == 0 {
		return
	}
	actions, err := m.cfg.updateResolver.Resolve(ctx, ResolveBatch{
		NewFacts:   newFacts,
		Candidates: candidates,
	})
	if err != nil {
		m.log("ltm: resolver Resolve failed: %v", err)
		resolverActionsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("op", "error")))
		return
	}
	if len(actions) == 0 {
		resolverActionsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("op", "noop")))
		return
	}
	m.applyResolverActions(ctx, scope, newFacts, actions, now)
}

// gatherResolverCandidates issues one Recall per new fact and returns
// the union, deduplicated by entry ID and ordered by descending score
// (per-fact ranking is preserved on first occurrence). Recall errors
// are logged but do not abort the batch — the resolver still runs on
// whatever candidates were collected.
func (m *lt) gatherResolverCandidates(
	ctx context.Context, scope Scope, newFacts []ResolveNewFact,
) []Hit {
	seen := make(map[string]struct{})
	var out []Hit
	for _, nf := range newFacts {
		hits, err := m.Recall(ctx, scope, Request{
			Query: nf.Fact.Content,
			TopK:  m.cfg.resolverTopK,
		})
		if err != nil {
			m.log("ltm: resolver candidate Recall failed for %q: %v", nf.EntryID, err)
			continue
		}
		for _, h := range hits {
			if _, dup := seen[h.Entry.ID]; dup {
				continue
			}
			seen[h.Entry.ID] = struct{}{}
			out = append(out, h)
		}
	}
	return out
}

// applyResolverActions writes the supersede / tombstone metadata
// implied by the resolver's actions. Unknown SourceIDs are skipped
// (defensive against LLM hallucination); unknown TargetIDs trigger a
// no-op since they don't exist in the index.
func (m *lt) applyResolverActions(
	ctx context.Context, scope Scope,
	newFacts []ResolveNewFact, actions []ResolveAction, now time.Time,
) {
	knownSources := make(map[string]struct{}, len(newFacts))
	for _, nf := range newFacts {
		knownSources[nf.EntryID] = struct{}{}
	}
	ns := NamespaceFor(scope)
	for _, a := range actions {
		op := string(a.Op)
		switch a.Op {
		case OpUpdate, OpDelete:
			if a.TargetID == "" {
				continue
			}
			if _, ok := knownSources[a.SourceID]; !ok {
				resolverActionsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("op", "error")))
				m.log("ltm: resolver action references unknown source_id %q", a.SourceID)
				continue
			}
			if a.TargetID == a.SourceID {
				continue
			}
			doc, ok := m.fetchDoc(ctx, ns, a.TargetID)
			if !ok {
				continue
			}
			if doc.Metadata == nil {
				doc.Metadata = map[string]any{}
			}
			doc.Metadata[MetaSupersededBy] = a.SourceID
			doc.Metadata[MetaSupersededAt] = now.UnixMilli()
			if a.Op == OpDelete {
				doc.Metadata[MetaTombstone] = true
			}
			if err := m.idx.Upsert(ctx, ns, []retrieval.Doc{doc}); err != nil {
				m.log("ltm: resolver upsert failed for %q: %v", doc.ID, err)
				op = "error"
			} else {
				supersedeTotal.Add(ctx, 1,
					metric.WithAttributes(attribute.String("channel", "resolver")))
			}
		case OpAdd, OpNoop:
			// nothing to do — the new entry is written by the caller
		default:
			op = "error"
		}
		resolverActionsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("op", op)))
	}
}

// fetchDoc returns the doc with the given id from the namespace. The
// retrieval interface exposes Get only via the optional [DocGetter]
// sub-interface; when the backend does not implement it the resolver
// gracefully no-ops (the new entry is still written, only the
// supersede tagging is skipped). All in-tree backends (memory,
// postgres, qdrant adapter, journal wrapper) implement DocGetter so
// this fallback is rarely exercised in practice.
func (m *lt) fetchDoc(ctx context.Context, ns, id string) (retrieval.Doc, bool) {
	g, ok := m.idx.(retrieval.DocGetter)
	if !ok {
		return retrieval.Doc{}, false
	}
	d, found, err := g.Get(ctx, ns, id)
	if err != nil {
		m.log("ltm: resolver Get %q failed: %v", id, err)
		return retrieval.Doc{}, false
	}
	return d, found
}
