// Package bindings assembles host capabilities into a script.Env for any
// script.Runtime implementation (jsrt, luart, etc.).
//
// # Purpose
//
// bindings does two things: (1) expose Go-side interfaces as script-callable
// globals/tables; (2) provide common presets to reduce boilerplate.
// Business policy (who may call what, quotas, auth) belongs to the caller:
// pass pre-trimmed dependencies or tighten capabilities via Options.
// VM-agnostic: bindings never parses syntax; Lua/JS are handled by script sub-packages.
//
// # Dependency constraint
//
// bindings depends only on workflow and llm — never on graph.
// Type aliases in the graph package (graph.Board = workflow.Board, etc.) allow
// graph/node/scriptnode to pass *graph.Board to bindings without conversion.
//
// # Layering model
//
//  1. core: BindingFunc, BuildEnv — language-agnostic assembly primitives.
//  2. atomic bridges: one file per host capability, named New<Domain>Bridge;
//     the injected global is typically the lowercase domain name (board, fs, …).
//  3. expr subsystem: compiled-program LRU cache (see expr.go), transparent to scripts.
//  4. presets: common combinations as convenience functions; presets express
//     "default assembly" and do not replace caller-level policy.
//  5. LLM bridge (bridge_llm.go): calls llm.RunRound / llm.StreamRound and
//     returns structured results to scripts; does NOT write to the board
//     (scripts control data flow explicitly). Supports blocking llm.run()
//     and iterator-based llm.stream() modes.
//
// # Directory layout
//
//	doc.go            package-level design notes (this file)
//	core.go           BindingFunc, BuildEnv
//	bridge_board.go   workflow board variables
//	bridge_stream.go  streaming events (backed by workflow.StreamCallback)
//	bridge_expr.go    expr-lang expressions (eval)
//	bridge_shell.go   sandboxed subprocesses (allowlist via ShellOption)
//	bridge_fs.go      workspace files
//	bridge_runtime.go sub-script execScript (inherits parent bindings)
//	run.go            run metadata (run_id, task_id)
//	bridge_llm.go     NewLLMBridge → global "llm" (run / stream); delegates to llm.RunRound / llm.StreamRound
//	tools.go          tool.Registry (deny-by-default, explicit allowlist or AllowAll)
//	expr.go           LRU cache and evalExpr (unexported)
//	presets.go        AgentStepBindings and other combinations
//	*_test.go         table-driven / jsrt integration tests
//
// # Global naming convention
//
// Script global names match the name returned by BindingFunc: board, stream,
// expr, shell, fs, runtime (injected by scriptnode), run, tools, llm.
// Methods exposed by each bridge use lowercase_underscore style, consistent
// with existing script conventions.
//
// # Integration with Graph / workflow
//
//   - Graph ScriptNode: composes board+stream+expr+runtime in ExecuteBoard,
//     then appends shell/fs per node type (see graph/node/scriptnode).
//   - Workflow Agent: uses presets or manual composition; add NewLLMBridge when LLM access is needed.
//
// # Checklist for adding a new bridge
//
//  1. Are closure-captured dependencies thread-safe and context-cancellable?
//  2. Are defaults least-privilege (e.g. tools deny-by-default, shell recommends allowlist)?
//  3. Are return values stable on the script side (map field names, multi-return mapping in luart/jsrt)?
//  4. Should it be included in a preset? Document which execution paths it is compatible with.
//
// # Testing conventions
//
// Integration tests use jsrt to execute small script snippets that verify bindings;
// pure Go logic (e.g. LRU) is covered by *_test.go unit tests.
package bindings
