package recent

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	// DefaultSummaryDAGID is the descriptor ID used by NewSummaryDAG unless overridden.
	DefaultSummaryDAGID views.ID = "summary-dag"

	// DefaultSummaryDAGVersion is the descriptor version used by NewSummaryDAG unless overridden.
	DefaultSummaryDAGVersion = "v1"
)

// SummaryDAG is a lightweight facade for the summary DAG view contract.
//
// It stores derived summary nodes for long-context compression. The persisted
// nodes are rebuildable from MessageLog evidence and are not a canonical message
// store.
type SummaryDAG struct {
	store   SummaryStore
	id      views.ID
	version string
}

var _ views.View = (*SummaryDAG)(nil)

// NewSummaryDAG creates a SummaryDAG view backed by store.
func NewSummaryDAG(store SummaryStore, opts ...SummaryDAGOption) *SummaryDAG {
	dag := &SummaryDAG{
		store:   store,
		id:      DefaultSummaryDAGID,
		version: DefaultSummaryDAGVersion,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applySummaryDAG(dag)
		}
	}
	return dag
}

// Descriptor declares the SummaryDAG view identity.
func (d *SummaryDAG) Descriptor() views.Descriptor {
	return views.Descriptor{
		ID:      d.id,
		Kind:    views.KindSummaryDAG,
		Version: d.version,
	}
}

// PutNode stores or replaces a summary node.
func (d *SummaryDAG) PutNode(ctx context.Context, node SummaryNode) (SummaryNode, error) {
	if d.store == nil {
		return SummaryNode{}, errdefs.Validationf("%s: store is required", summaryDAGErrPrefix)
	}
	return d.store.PutNode(ctx, node)
}

// GetNode returns one summary node by scope and node id.
func (d *SummaryDAG) GetNode(ctx context.Context, scope views.Scope, id NodeID) (SummaryNode, bool, error) {
	if d.store == nil {
		return SummaryNode{}, false, errdefs.Validationf("%s: store is required", summaryDAGErrPrefix)
	}
	return d.store.GetNode(ctx, scope, id)
}

// ListNodes returns summary nodes ordered by ascending node id.
func (d *SummaryDAG) ListNodes(ctx context.Context, scope views.Scope, opts ListOptions) ([]SummaryNode, error) {
	if d.store == nil {
		return nil, errdefs.Validationf("%s: store is required", summaryDAGErrPrefix)
	}
	return d.store.ListNodes(ctx, scope, opts)
}

// DeleteScope removes all summary nodes for a scope.
func (d *SummaryDAG) DeleteScope(ctx context.Context, scope views.Scope) error {
	if d.store == nil {
		return errdefs.Validationf("%s: store is required", summaryDAGErrPrefix)
	}
	return d.store.DeleteScope(ctx, scope)
}

// DeleteNode removes one summary node by scope and node id when the backing
// store supports targeted deletion.
func (d *SummaryDAG) DeleteNode(ctx context.Context, scope views.Scope, id NodeID) error {
	if d.store == nil {
		return errdefs.Validationf("%s: store is required", summaryDAGErrPrefix)
	}
	deleter, ok := d.store.(SummaryNodeDeleter)
	if !ok {
		return errdefs.NotAvailablef("%s: store does not support targeted node delete", summaryDAGErrPrefix)
	}
	return deleter.DeleteNode(ctx, scope, id)
}
