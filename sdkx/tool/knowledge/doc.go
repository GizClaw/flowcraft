// Package knowledge ships the LLM-callable tool wrappers around
// sdk/knowledge.Service. It is the v0.3.0 home of NewSearchServiceTool
// and NewPutServiceTool, which currently live in sdk/knowledge with a
// // Deprecated: marker pointing here.
//
// Deprecated: memory-domain tool adapters are moving to
// github.com/GizClaw/flowcraft/memory. This package will be removed in v0.5.0.
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
// Both helpers mirror the sdk/knowledge versions verbatim. Function
// signatures, tool names, JSON shapes, default values, and error
// codes are preserved across the move; the migration is a pure
// import-path swap:
//
//	-import "github.com/GizClaw/flowcraft/sdk/knowledge"
//	+import knowledgetool "github.com/GizClaw/flowcraft/sdkx/tool/knowledge"
//
//	- search := knowledge.NewSearchServiceTool(svc)
//	+ search := knowledgetool.NewSearchServiceTool(svc)
package knowledge
