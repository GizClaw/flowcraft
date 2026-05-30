package assembly

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/knowledge"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/ops"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/vessel"
)

type WorkspaceHandle = workspace.Workspace
type RecallHandle = recall.Memory
type KnowledgeHandle = *knowledge.Service

// Assembly is the resource graph produced by Build.
type Assembly struct {
	Manifest Manifest
	Captain  *vessel.Captain

	Workspace WorkspaceHandle
	Recall    RecallHandle
	Knowledge KnowledgeHandle
	Tools     *tool.Registry

	OpsRunner     *ops.Runner
	OpsSupervisor *ops.Supervisor
	opsTarget     ops.Target

	closers []func(context.Context) error
}
