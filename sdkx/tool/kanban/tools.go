package kanban

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkkanban "github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// SubmitName and TaskContextName are the canonical tool ids. They are
// stable across versions so prompts naming the tools keep working.
const (
	SubmitName      = "kanban_submit"
	TaskContextName = "task_context"
)

// SubmitTool lets a model delegate work to another agent by putting a
// card on the shared board.
//
// The board is supplied either at construction, via the Kanban field,
// or per call from the context via [WithKanban] — for hosts that
// register tools into a registry before the board exists.
type SubmitTool struct {
	Kanban *sdkkanban.Kanban
}

// Definition implements [tool.Tool].
//
// The description promises only what the board guarantees: the card is
// queued. It deliberately does not promise a callback. Whether anything
// executes the card, and how the result comes back, is the host's
// wiring — a tool that pledged delivery would be lying whenever no
// executor is attached, and a model told to expect a callback that
// never arrives waits forever.
func (t *SubmitTool) Definition() tool.Definition {
	return tool.DefineSchema(
		SubmitName,
		"Delegate a task to another agent. The task is queued on the shared board and "+
			"returns a card_id immediately; it does not run during this call. "+
			"Use "+TaskContextName+" with the card_id to check progress and read the result once finished.",
		tool.Property("target_agent_id", "string",
			"ID of the agent that should perform the task."),
		tool.Property("query", "string",
			"The instruction for the target agent."),
		tool.Property("user_query", "string",
			"The user's original request that led to this delegation, so the target agent can see the intent behind the instruction."),
		tool.Property("dispatch_note", "string",
			"A note to your future self: why you delegated this and what you expect back."),
	).Required("target_agent_id", "query").Build()
}

// Metadata implements [tool.ToolMetadata]. Submitting places a card on
// a shared board, which every other participant can observe.
func (t *SubmitTool) Metadata() tool.ToolMeta {
	return tool.ToolMeta{MutatesState: true}
}

// Execute implements [tool.Tool].
func (t *SubmitTool) Execute(ctx context.Context, arguments string) (string, error) {
	k := t.resolve(ctx)
	if k == nil {
		return "", errdefs.NotAvailablef(
			"%s: no kanban board available; pass one via the Kanban field or WithKanban", SubmitName,
		)
	}

	var args struct {
		TargetAgentID string `json:"target_agent_id"`
		Query         string `json:"query"`
		UserQuery     string `json:"user_query"`
		DispatchNote  string `json:"dispatch_note"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("%s: invalid arguments: %w", SubmitName, err)
	}

	card, err := k.Submit(ctx, sdkkanban.Task{
		TargetAgentID: args.TargetAgentID,
		Query:         args.Query,
		UserQuery:     args.UserQuery,
		DispatchNote:  args.DispatchNote,
	})
	if err != nil {
		return "", err
	}

	out, _ := json.Marshal(map[string]string{
		"card_id":         card.ID,
		"status":          string(card.Status),
		"target_agent_id": args.TargetAgentID,
		"message": "Task queued on the board. Check " + TaskContextName +
			" with this card_id for progress and the result.",
	})
	return string(out), nil
}

func (t *SubmitTool) resolve(ctx context.Context) *sdkkanban.Kanban {
	if t.Kanban != nil {
		return t.Kanban
	}
	return KanbanFrom(ctx)
}

// TaskContextTool reads a card back: what was asked, why, and how it
// turned out. It is the counterpart to [SubmitTool] — the model
// delegates, then comes back to this tool to find out what happened.
type TaskContextTool struct {
	Kanban *sdkkanban.Kanban
}

// Definition implements [tool.Tool].
func (t *TaskContextTool) Definition() tool.Definition {
	return tool.DefineSchema(
		TaskContextName,
		"Read the full context of a task you delegated: the original request, your dispatch "+
			"note, the instruction, and its current status or result. Call this to check whether "+
			"a delegated task has finished and what it produced.",
		tool.Property("card_id", "string",
			"The card_id returned by "+SubmitName+"."),
	).Required("card_id").Build()
}

// Execute implements [tool.Tool].
func (t *TaskContextTool) Execute(ctx context.Context, arguments string) (string, error) {
	k := t.resolve(ctx)
	if k == nil {
		return "", errdefs.NotAvailablef(
			"%s: no kanban board available; pass one via the Kanban field or WithKanban", TaskContextName,
		)
	}

	var args struct {
		CardID string `json:"card_id"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("%s: invalid arguments: %w", TaskContextName, err)
	}

	card, ok := k.Card(args.CardID)
	if !ok {
		return "", errdefs.NotFoundf("%s: no card %q on the board", TaskContextName, args.CardID)
	}
	return card.TaskContext(), nil
}

func (t *TaskContextTool) resolve(ctx context.Context) *sdkkanban.Kanban {
	if t.Kanban != nil {
		return t.Kanban
	}
	return KanbanFrom(ctx)
}

// ctxKey is private so no other package can collide with our context
// key by accident.
type ctxKey int

const ctxKeyKanban ctxKey = iota

// WithKanban attaches a board to ctx so the tools can resolve one
// without a struct field. Hosts that build their tool registry before
// the board exists use this.
func WithKanban(ctx context.Context, k *sdkkanban.Kanban) context.Context {
	return context.WithValue(ctx, ctxKeyKanban, k)
}

// KanbanFrom returns the board installed by [WithKanban], or nil.
func KanbanFrom(ctx context.Context) *sdkkanban.Kanban {
	k, _ := ctx.Value(ctxKeyKanban).(*sdkkanban.Kanban)
	return k
}

// Compile-time interface checks.
var (
	_ tool.Tool         = (*SubmitTool)(nil)
	_ tool.Tool         = (*TaskContextTool)(nil)
	_ tool.ToolMetadata = (*SubmitTool)(nil)
)
