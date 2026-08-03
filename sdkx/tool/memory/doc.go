// Package memory exposes the memory operations that integrate with
// the LLM tool surface, namely Import. Each op that maps to a
// tool gets a *tool.Tool constructor that wraps a
// *memory.Runtime.
//
// # Why no deploy factory
//
// Other memory surfaces (Load, Recall, Append) integrate through
// the deploy hook factories registered on a Builder. Import is
// different: it is a single, named, callable action the LLM asks
// for on demand, not a per-Run fixed step. The natural shape is
// therefore a *tool.Tool that a host wires into the same
// *tool.Registry the agent already consults.
//
// To register the tool, the host calls RegisterImportTool:
//
//	rt, _ := config.NewAssembly(ctx, doc, deps)
//	reg := tool.NewRegistry()
//	memtool.RegisterImportTool(reg, rt, memtool.ImportSettings{
//	    Scope:     memtool.ScopeConfig{RuntimeID: "prod"},
//	    DatasetID: "knowledge",
//	})
//	deployBuilder.RegisterSource("host.tools", func(...) (any, error) {
//	    return reg, nil
//	})
//
// The deploy document then references the tool by name in the
// agent's allow-list:
//
//	agents:
//	  a:
//	    engine: {kind: graph}
//	    tools: [memory.import]
//
// Per-deployment settings live in memory.yaml; the tool
// constructor reads them from the host. This split keeps the
// deploy document free of tool-specific configuration while
// letting the host pre-wire a tool with the right scope and
// dataset id at startup time.
package memory
