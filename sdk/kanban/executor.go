package kanban

import "context"

// AgentExecutor is the interface for executing tasks on target agents.
// WorkflowRunner implements this by creating/reusing AgentActors.
type AgentExecutor interface {
	// ExecuteTask runs a task targeting a specific agent within a board scope.
	// It claims the card, executes asynchronously, and calls Done/Fail on completion.
	ExecuteTask(ctx context.Context, scopeID, targetAgentID string, card *Card, query string, inputs map[string]any) error
}
