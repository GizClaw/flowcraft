package executor

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/graph"
)

type parallelBranchInfoCtxKey struct{}

type parallelBranchInfo struct {
	ForkID      string
	BranchID    string
	Speculative bool
}

func withParallelBranchInfo(ctx context.Context, info parallelBranchInfo) context.Context {
	if info.ForkID == "" && info.BranchID == "" && !info.Speculative {
		return ctx
	}
	return context.WithValue(ctx, parallelBranchInfoCtxKey{}, info)
}

func parallelBranchInfoFromContext(ctx context.Context) (parallelBranchInfo, bool) {
	info, ok := ctx.Value(parallelBranchInfoCtxKey{}).(parallelBranchInfo)
	return info, ok
}

// newNodePublisher builds the StreamPublisher handed to a node. The
// node speaks the simplified (eventType, payload) shape; this wrapper
// translates each emit into a fully-formed engine event.Envelope and
// pushes it through the executor's host publisher.
//
// agentID is resolved once (from engine.Run.Attributes /
// WithActorKey ctx-key fallback) so every emit pays a constant cost
// rather than re-walking ctx for each payload. The stepActor segment
// stamped onto the subject is "<agentID>.node.<nodeID>" — the
// envelope.HeaderAgentID and HeaderNodeID transports carry the two
// dimensions separately for header-routed subscribers.
//
// The wrapper is always non-nil so nodes can call ctx.Publisher.Emit
// without nil-checks.
func newNodePublisher(ctx context.Context, cfg runConfig, nodeID string) graph.StreamPublisher {
	agentID := agentIDFor(ctx, cfg)
	stepActor := stepActorFor(agentID, nodeID)
	graphName := cfg.graphName
	pub := cfg.publisher
	branchInfo, inParallelBranch := parallelBranchInfoFromContext(ctx)

	return graph.StreamPublisherFunc(func(eventType string, payload any) {
		if pub == nil {
			return
		}
		if inParallelBranch && ctx.Err() != nil {
			return
		}
		pl := normalisePayload(eventType, payload)
		if inParallelBranch {
			pl["speculative"] = branchInfo.Speculative
			pl["fork_id"] = branchInfo.ForkID
			pl["branch_id"] = branchInfo.BranchID
		}
		publishNodeEvent(ctx, pub, engine.SubjectStreamDelta(cfg.runID, stepActor),
			cfg.runID, graphName, agentID, nodeID, pl)
	})
}

// normalisePayload guarantees a map shape with a "type" field so
// subject-only subscribers can still discriminate without inspecting
// the Subject suffix.
func normalisePayload(eventType string, payload any) map[string]any {
	out := map[string]any{"type": eventType}
	if m, ok := payload.(map[string]any); ok {
		for k, v := range m {
			if k == "type" {
				continue
			}
			out[k] = v
		}
	} else if payload != nil {
		out["payload"] = payload
	}
	return out
}
