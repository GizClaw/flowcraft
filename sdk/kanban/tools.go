package kanban

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// SubmitTool allows LLM to submit tasks to the Kanban board.
// Kanban is injected via struct field (when the instance is known at construction)
// or resolved from context at execution time via KanbanFrom.
type SubmitTool struct {
	Kanban *Kanban
}

func (t *SubmitTool) Definition() model.ToolDefinition {
	return tool.DefineSchema("kanban_submit",
		"Dispatch a task to another agent. Returns a card_id. The agent executes in the background and the system delivers the result via a [Task Callback] message when done. "+
			"You can optionally schedule the task with a delay or cron expression.",
		tool.Property("target_agent_id", "string", "Target agent ID to execute the task"),
		tool.Property("query", "string", "Specific task instruction for the target agent"),
		tool.Property("user_query", "string",
			"The user's original request that triggered this task"),
		tool.Property("dispatch_note", "string",
			"Brief note about the task purpose and how to summarize the result for the user"),
		tool.Property("delay", "string",
			"Execute after a delay instead of immediately. Go duration format, e.g. \"30s\", \"5m\", \"2h\""),
		tool.Property("cron", "string",
			"Execute on a recurring cron schedule. 5-field cron expression (minute hour day month weekday). Examples: \"0 9 * * MON-FRI\" = weekdays 9AM, \"*/30 * * * *\" = every 30 minutes"),
		tool.Property("timezone", "string",
			"Timezone for cron schedule, IANA format. e.g. \"Asia/Shanghai\", \"America/New_York\". Defaults to UTC"),
	).Required("target_agent_id", "query").Build()
}

func (t *SubmitTool) Execute(ctx context.Context, arguments string) (string, error) {
	k := t.resolve(ctx)
	if k == nil {
		return "", errdefs.NotAvailablef("kanban_submit: no kanban instance available")
	}

	var args struct {
		TargetAgentID string `json:"target_agent_id"`
		Query         string `json:"query"`
		UserQuery     string `json:"user_query"`
		DispatchNote  string `json:"dispatch_note"`
		Delay         string `json:"delay"`
		Cron          string `json:"cron"`
		Timezone      string `json:"timezone"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("kanban_submit: invalid arguments: %w", err)
	}

	cardID, err := k.Submit(ctx, TaskOptions{
		TargetAgentID: args.TargetAgentID,
		Query:         args.Query,
		UserQuery:     args.UserQuery,
		DispatchNote:  args.DispatchNote,
		Delay:         args.Delay,
		Cron:          args.Cron,
		Timezone:      args.Timezone,
	})
	if err != nil {
		return "", err
	}

	status := "submitted"
	message := "Task submitted. The target agent is executing in the background. A [Task Callback] message will be delivered when done."
	idLabel := "card_id"
	if args.Cron != "" {
		status = "scheduled"
		message = fmt.Sprintf("Recurring task scheduled (cron: %s). The task will be submitted automatically on each trigger.", args.Cron)
		idLabel = "schedule_id"
	} else if args.Delay != "" {
		status = "delayed"
		message = fmt.Sprintf("Task will be submitted after %s delay.", args.Delay)
		idLabel = "timer_id"
	}

	out, _ := json.Marshal(map[string]string{
		idLabel:           cardID,
		"status":          status,
		"target_agent_id": args.TargetAgentID,
		"message":         message,
	})
	return string(out), nil
}

// TaskContextTool allows the Dispatcher to retrieve the full context of a
// previously dispatched async task, including the original user request,
// dispatch note, task instruction, and execution result.
type TaskContextTool struct {
	Kanban *Kanban
}

func (t *TaskContextTool) Definition() model.ToolDefinition {
	return tool.DefineSchema("task_context",
		"Retrieve the full context of a dispatched task, including original user request, "+
			"your dispatch note, task instruction, and execution result. "+
			"Use this when you receive a task callback and need to recall the original context.",
		tool.Property("card_id", "string", "The card ID from the task callback message"),
	).Required("card_id").Build()
}

func (t *TaskContextTool) Execute(ctx context.Context, arguments string) (string, error) {
	k := t.resolve(ctx)
	if k == nil {
		return "", errdefs.NotAvailablef("task_context: no kanban instance available")
	}

	var args struct {
		CardID string `json:"card_id"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("task_context: invalid arguments: %w", err)
	}

	card, err := k.GetCard(ctx, args.CardID)
	if err != nil {
		return "", fmt.Errorf("task_context: %w", err)
	}

	return BuildTaskContext(card), nil
}

func (t *TaskContextTool) resolve(ctx context.Context) *Kanban {
	if t.Kanban != nil {
		return t.Kanban
	}
	return KanbanFrom(ctx)
}

// resolve returns the Kanban instance: struct field first, context fallback.
func (t *SubmitTool) resolve(ctx context.Context) *Kanban {
	if t.Kanban != nil {
		return t.Kanban
	}
	return KanbanFrom(ctx)
}

func WithKanban(ctx context.Context, k *Kanban) context.Context {
	return context.WithValue(ctx, ctxKeyKanban, k)
}

func KanbanFrom(ctx context.Context) *Kanban {
	k, _ := ctx.Value(ctxKeyKanban).(*Kanban)
	return k
}
