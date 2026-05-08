// Package history ships the LLM-callable tool wrappers around
// sdk/history.Coordinator. It is the v0.3.0 home of [ToolDeps]
// and [RegisterTools], which currently live in sdk/history with
// // Deprecated: markers pointing here.
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
// During the v0.2.x → v0.3.0 transition this package is a thin
// re-export layer over sdk/history because the underlying tool
// implementation depends on package-private archive helpers
// (loadManifestImpl, archiveImpl, ...) that cannot be reached from
// outside sdk/history. ToolDeps is a Go type alias to
// [sdkhistory.ToolDeps] so the structures are interchangeable; the
// migration is a pure import-path swap:
//
//	-import "github.com/GizClaw/flowcraft/sdk/history"
//	+import historytool "github.com/GizClaw/flowcraft/sdkx/tool/history"
//
//	- history.RegisterTools(reg, history.ToolDeps{...})
//	+ historytool.RegisterTools(reg, historytool.ToolDeps{...})
//
// At sdk/v0.3.0 the sdk-side helpers and their private dependencies
// relocate into this package, and the alias is replaced with a
// first-class type definition.
package history
