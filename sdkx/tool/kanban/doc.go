// Package kanban ships the LLM-callable tool wrappers around
// sdk/kanban.Kanban. It is the v0.3.0 home of [SubmitTool],
// [TaskContextTool], [WithKanban] and [KanbanFrom], which currently
// live in sdk/kanban with // Deprecated: markers pointing here.
//
// # Why sdkx
//
// sdk defines interfaces and primitives; sdkx ships concrete adapters
// that integrate with external systems or external protocol specs.
// LLM tool implementations are concrete adapters — they bridge the
// generic [tool.Tool] interface to one specific service — and
// therefore belong here, mirroring the existing sdk/llm → sdkx/llm/...
// layout. See docs/migrations/v0.3.0.md.
//
// # Surface
//
// Mirrors the sdk/kanban versions verbatim. Tool names, JSON shapes,
// behaviour, and error codes are preserved across the move; the
// migration is a pure import-path swap:
//
//	-import "github.com/GizClaw/flowcraft/sdk/kanban"
//	+import kanbantool "github.com/GizClaw/flowcraft/sdkx/tool/kanban"
//
//	- ctx = kanban.WithKanban(ctx, k)
//	+ ctx = kanbantool.WithKanban(ctx, k)
//
// # Context-key compatibility
//
// During the v0.2.x → v0.3.0 transition both sdk/kanban.WithKanban
// and this package's WithKanban interoperate: the sdkx versions are
// thin re-exports of the sdk functions so a [SubmitTool] reading
// from ctx finds a [*kanban.Kanban] that was installed via either
// API. At sdk/v0.3.0 the sdk-side helpers are deleted; the sdkx
// versions become the canonical implementation.
package kanban
