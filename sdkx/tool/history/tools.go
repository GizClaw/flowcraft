package history

import (
	sdkhistory "github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// ToolDeps bundles everything the history_expand / history_compact
// tools need at registration time. Pass it to [RegisterTools].
//
// This is a Go type alias to [sdkhistory.ToolDeps] — instances of
// either type are interchangeable. At sdk/v0.3.0 this becomes a
// first-class type definition once the sdk-side helpers are deleted.
//
// See [sdkhistory.ToolDeps] for field semantics.
type ToolDeps = sdkhistory.ToolDeps

// RegisterTools registers history_expand and history_compact against
// the supplied [tool.Registry]. The summary index is auto-injected
// into the LLM system prompt via the workflow.VarSummaryIndex board
// variable, so a separate history_search tool is not needed.
//
// During the v0.2.x → v0.3.0 transition this delegates to
// [sdkhistory.RegisterTools] because the tool implementation
// reaches into package-private archive helpers in sdk/history. At
// sdk/v0.3.0 the implementation relocates into this package.
func RegisterTools(registry *tool.Registry, deps ToolDeps) {
	sdkhistory.RegisterTools(registry, deps)
}
