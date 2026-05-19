// Package retrieval implements the canonical retrieval projection.
//
// It is a write-time derived view of the temporal ledger: facts are
// flattened into retrieval.Doc shape and pushed into a retrieval.Index
// backend. Recall reads from the index through a CandidateSource, never
// the other way around — retrieval here is a search backend, not a
// truth layer (docs §8.1).
package retrieval

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// Projection is the canonical retrieval projection. It is Required:
// Save must not ack a write that is not searchable, otherwise the
// read path will silently miss freshly written facts.
type Projection struct {
	index retrieval.Index
}

// New constructs a retrieval Projection backed by index. Index ownership
// stays with the caller (Memory.Close closes the index, not the
// projection).
func New(index retrieval.Index) (*Projection, error) {
	if index == nil {
		return nil, fmt.Errorf("recall retrieval projection: index is required")
	}
	return &Projection{index: index}, nil
}

// Name implements projection.Projection.
func (p *Projection) Name() string { return "retrieval" }

// Consistency reports Required: a retrieval projection failure must
// fail the canonical write so callers do not see an empty Recall on
// a fact they just stored.
func (p *Projection) Consistency() projection.Consistency { return projection.Required }

// Project upserts canonical facts into the retrieval namespace. Facts
// in mixed scopes are grouped per namespace so each Upsert is scope
// local.
func (p *Projection) Project(ctx context.Context, facts []model.TemporalFact) error {
	if len(facts) == 0 {
		return nil
	}
	grouped := groupByNamespace(facts)
	for ns, group := range grouped {
		docs := make([]retrieval.Doc, 0, len(group))
		for _, f := range group {
			docs = append(docs, toDoc(f))
		}
		if err := p.index.Upsert(ctx, ns, docs); err != nil {
			return fmt.Errorf("retrieval projection upsert ns=%s: %w", ns, err)
		}
	}
	return nil
}

// Forget removes facts by id within a scope-derived namespace.
func (p *Projection) Forget(ctx context.Context, scope model.Scope, factIDs []string) error {
	if len(factIDs) == 0 {
		return nil
	}
	ns := NamespaceFor(scope)
	if err := p.index.Delete(ctx, ns, factIDs); err != nil {
		return fmt.Errorf("retrieval projection delete ns=%s: %w", ns, err)
	}
	return nil
}

// Rebuild applies an exact-replace semantics within the supplied
// scope: docs present in the index but missing from the snapshot are
// deleted, then the snapshot is upserted. This is the canonical
// recovery operation for projection drift (docs §8) and matches the
// invariant that projections are rebuildable views of the ledger.
//
// Note: facts in the snapshot must all share scope. Multi-scope
// snapshots should be split by the caller (Memory.Rebuild handles
// this internally).
func (p *Projection) Rebuild(ctx context.Context, scope model.Scope, facts []model.TemporalFact) error {
	ns := NamespaceFor(scope)
	snapshotIDs := make(map[string]struct{}, len(facts))
	for _, f := range facts {
		if f.ID != "" {
			snapshotIDs[f.ID] = struct{}{}
		}
	}

	existing, err := listAllDocIDs(ctx, p.index, ns)
	if err != nil {
		return fmt.Errorf("retrieval projection rebuild ns=%s list: %w", ns, err)
	}
	var stale []string
	for _, id := range existing {
		if _, keep := snapshotIDs[id]; !keep {
			stale = append(stale, id)
		}
	}
	if len(stale) > 0 {
		if err := p.index.Delete(ctx, ns, stale); err != nil {
			return fmt.Errorf("retrieval projection rebuild ns=%s delete stale: %w", ns, err)
		}
	}
	if len(facts) == 0 {
		return nil
	}
	return p.Project(ctx, facts)
}

// listAllDocIDs paginates retrieval.Index.List to enumerate every
// doc id currently stored in namespace. We page in modest batches so
// large namespaces do not require backend-specific scan support.
func listAllDocIDs(ctx context.Context, idx retrieval.Index, namespace string) ([]string, error) {
	const pageSize = 256
	var (
		out  []string
		page string
	)
	for {
		resp, err := idx.List(ctx, namespace, retrieval.ListRequest{
			PageSize:  pageSize,
			PageToken: page,
			OrderBy:   retrieval.OrderByIDAsc,
		})
		if err != nil {
			return nil, err
		}
		if resp == nil {
			break
		}
		for _, d := range resp.Items {
			out = append(out, d.ID)
		}
		if resp.NextPageToken == "" {
			break
		}
		page = resp.NextPageToken
	}
	return out, nil
}

func groupByNamespace(facts []model.TemporalFact) map[string][]model.TemporalFact {
	out := make(map[string][]model.TemporalFact)
	for _, f := range facts {
		ns := NamespaceFor(f.Scope)
		out[ns] = append(out[ns], f)
	}
	return out
}

// toDoc flattens a fact into a retrieval Doc. Reserved metadata keys
// are owned by the projection: user-supplied Metadata is copied first
// so reserved keys always win when there is overlap.
func toDoc(f model.TemporalFact) retrieval.Doc {
	meta := make(map[string]any, len(f.Metadata)+12)
	for k, v := range f.Metadata {
		meta[k] = v
	}

	meta[model.MetaFactID] = f.ID
	meta[model.MetaFactKind] = string(f.Kind)
	meta[model.MetaScopeRT] = f.Scope.RuntimeID
	meta[model.MetaScopeUser] = f.Scope.UserID
	meta[model.MetaScopeAgent] = f.Scope.AgentID
	meta[model.MetaMergeKey] = f.MergeKey
	meta[model.MetaConfidence] = f.Confidence
	meta[model.MetaObservedAt] = f.ObservedAt.UnixMilli()
	if f.ValidFrom != nil {
		meta[model.MetaValidFrom] = f.ValidFrom.UnixMilli()
	}
	if f.ValidTo != nil {
		meta[model.MetaValidTo] = f.ValidTo.UnixMilli()
	}
	if f.CorrectedBy != "" {
		meta[model.MetaCorrectedBy] = f.CorrectedBy
	}
	if len(f.Entities) > 0 {
		entities := make([]any, len(f.Entities))
		for i, e := range f.Entities {
			entities[i] = e
		}
		meta[model.MetaEntities] = entities
	}

	return retrieval.Doc{
		ID:        f.ID,
		Content:   buildContent(f),
		Metadata:  meta,
		Timestamp: pickTimestamp(f),
	}
}

// buildContent renders the searchable text for a fact. For relation /
// state / preference facts we surface subject/predicate/object so BM25
// matches a "X spouse Y" style query even when Content was left empty
// by the caller.
func buildContent(f model.TemporalFact) string {
	if f.Content != "" {
		return f.Content
	}
	parts := make([]string, 0, 3)
	if f.Subject != "" {
		parts = append(parts, f.Subject)
	}
	if f.Predicate != "" {
		parts = append(parts, f.Predicate)
	}
	if f.Object != "" {
		parts = append(parts, f.Object)
	}
	if len(parts) == 0 {
		return f.EvidenceText
	}
	return strings.Join(parts, " ")
}

// pickTimestamp resolves Doc.Timestamp from valid_from -> observed_at.
// Phase 1 keeps this deterministic; richer time semantics arrive when
// the timeline projection lands in Phase 6.
func pickTimestamp(f model.TemporalFact) time.Time {
	if f.ValidFrom != nil && !f.ValidFrom.IsZero() {
		return *f.ValidFrom
	}
	return f.ObservedAt
}
