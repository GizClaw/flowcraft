package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
)

// NewRunInfoBridge exposes read-only run identity to scripts as
// global "run". The identity is AMBIENT: it is read from the context
// the EnvBuilder was built with (agent.WithRunInfo), not wired as a
// constructor parameter — the script node simply builds the env with
// the node's execution context and the bridge finds the identity
// there.
//
// Script-facing API:
//
//	run.get_run_id()         string  // agent.RunInfo.RunID
//	run.get_task_id()        string  // agent.RunInfo.TaskID
//	run.get_agent_id()       string  // agent.RunInfo.AgentID
//	run.get_context_id()     string  // agent.RunInfo.ConversationID
//	                                 // (A2A contextId semantics)
//	run.get_parent_run_id()  string  // agent.RunInfo.ParentRunID
//
// All getters return the empty string when the identity is unset (or
// the context carried no RunInfo at all), so scripts can do
// `if (!run.get_task_id()) { … }` to branch on absence without a
// separate "has_*" probe.
func NewRunInfoBridge() BindingFunc {
	return func(ctx context.Context) (string, any) {
		info, _ := agent.RunInfoFromContext(ctx)
		return "run", map[string]any{
			"get_run_id":        func() string { return info.RunID },
			"get_task_id":       func() string { return info.TaskID },
			"get_agent_id":      func() string { return info.AgentID },
			"get_context_id":    func() string { return info.ConversationID },
			"get_parent_run_id": func() string { return info.ParentRunID },
		}
	}
}
