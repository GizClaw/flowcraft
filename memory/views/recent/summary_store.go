package recent

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const summaryDAGErrPrefix = "memory/views/recent/summarydag"

// ListOptions controls deterministic summary-node scans within a conversation.
type ListOptions struct {
	AfterID NodeID
	Limit   int
	Level   *int
}

// SummaryStore persists SummaryDAG nodes.
type SummaryStore interface {
	PutNode(ctx context.Context, node SummaryNode) (SummaryNode, error)
	GetNode(ctx context.Context, scope views.Scope, id NodeID) (SummaryNode, bool, error)
	ListNodes(ctx context.Context, scope views.Scope, opts ListOptions) ([]SummaryNode, error)
	DeleteScope(ctx context.Context, scope views.Scope) error
}

func validateSummaryNode(node SummaryNode) error {
	if node.ID == "" {
		return errdefs.Validationf("%s: summary node id is required", summaryDAGErrPrefix)
	}
	if err := node.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid scope: %w", summaryDAGErrPrefix, err)
	}
	if node.Scope.ConversationID == "" {
		return errdefs.Validationf("%s: conversation_id is required", summaryDAGErrPrefix)
	}
	if node.Summary == "" {
		return errdefs.Validationf("%s: summary is required", summaryDAGErrPrefix)
	}
	if node.Level < 0 {
		return errdefs.Validationf("%s: level must be non-negative", summaryDAGErrPrefix)
	}
	if len(node.SourceRefs) == 0 {
		return errdefs.Validationf("%s: source_refs are required", summaryDAGErrPrefix)
	}
	for i, ref := range node.SourceRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
		if ref.Kind != views.SourceMessage {
			return errdefs.Validationf("%s: source_refs[%d] must reference messages", summaryDAGErrPrefix, i)
		}
	}
	if len(node.Signature.SourceRevisions) == 0 {
		return errdefs.Validationf("%s: source revisions are required", summaryDAGErrPrefix)
	}
	for i, rev := range node.Signature.SourceRevisions {
		if rev.Kind != views.SourceMessage {
			return errdefs.Validationf("%s: source revisions[%d] must reference messages", summaryDAGErrPrefix, i)
		}
	}
	if len(node.Signature.UpstreamViewRefs) > 0 {
		return errdefs.Validationf("%s: upstream view refs are not part of summary dag lineage", summaryDAGErrPrefix)
	}
	if err := node.Signature.Validate(); err != nil {
		return err
	}
	return nil
}
