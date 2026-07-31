// Package kanban ships the LLM-callable tool wrappers around
// [kanban.Kanban]: [SubmitTool] to dispatch work to another agent,
// [TaskContextTool] to read a dispatched task's full context, and
// [WithKanban]/[KanbanFrom] to carry the board on a context.
//
// # Primitive category: orchestration mechanism
//
// These tools ship built-in because they operate on the engine's own
// delegation state. kanban_submit dispatches work to another agent and
// task_context reads back the resulting card — both require the
// in-process [kanban.Kanban] the run is executing against. An
// out-of-process MCP server has no view of that board, so it could
// neither create a card the engine will schedule nor read one it
// already ran. See sdkx/tool's package doc for the boundary this sits
// on.
//
// # Why sdkx
//
// sdk defines interfaces and primitives; sdkx ships concrete adapters
// that integrate with external systems or external protocol specs.
// LLM tool implementations are concrete adapters — they bridge the
// generic [tool.Tool] interface to one specific service — and
// therefore belong here, mirroring sdk/inference → sdkx/inference/*.
//
// # Wiring
//
// Both tools resolve the board from the context, so a host installs it
// once per run rather than threading it through every construction:
//
//	ctx = kanban.WithKanban(ctx, board)
//	reg.Register(&kanban.SubmitTool{})
//	reg.Register(&kanban.TaskContextTool{})
//
// A tool invoked without a board on the context returns
// errdefs.NotAvailable rather than failing silently — the honest
// answer when the run has no delegation surface.
package kanban
