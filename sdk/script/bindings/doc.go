// Package bindings assembles host capabilities into a script.Env for any
// script.Runtime implementation (jsrt, luart, etc.).
//
// # Purpose
//
// bindings exposes Go-side interfaces as script-callable globals/tables.
// Business policy (who may call what, quotas, auth) belongs to the caller:
// pass pre-trimmed dependencies or tighten capabilities via Options.
// VM-agnostic: bindings never parses syntax; Lua/JS are handled by script sub-packages.
// Env assembly primitives (script.BindingFunc, script.LateBindingFunc, and
// script.EnvBuilder) live in sdk/script; this package keeps aliases and wrappers
// as a compatibility layer while owning concrete bridges.
//
// # Dependency constraint
//
// bindings depends on llm, model, tool, engine. It must never depend on
// graph: graph composes bindings, not the other way around. The Board
// surface bindings consume is the structural bindings.Board interface,
// which *engine.Board satisfies directly.
//
// # Layering model
//
//  1. core: compatibility aliases/wrappers for script BindingFunc/EnvBuilder.
//  2. atomic bridges: one file per host capability, named New<Domain>Bridge;
//     the injected global is typically the lowercase domain name (board, fs, …).
//  3. expr subsystem: compiled-program LRU cache (see bridge_expr.go), transparent to scripts.
//  4. presets: common combinations as convenience functions; presets express
//     "default assembly" and do not replace caller-level policy.
//  5. LLM bridge (bridge_llm.go + bridge_llm_round.go + llm_marshal.go):
//     drives an LLM round directly via llm.LLMResolver / llm.LLM /
//     tool.Registry — fully self-contained, no host runtime dependency.
//     Returns multimodal-aware structured results to scripts; does NOT
//     write to the board (scripts control data flow explicitly via the
//     board bridge). Supports blocking llm.run() and iterator-based
//     llm.stream() modes; the iterator exposes per-chunk model.Part
//     projections so scripts can branch on text / image / tool_call.
//
// # Directory layout
//
//	doc.go            package-level design notes (this file)
//	core.go           compatibility aliases for script.BindingFunc / EnvBuilder
//	bridge_board.go   board variables + typed message channels (engine.Board)
//	bridge_expr.go    expr-lang expressions (eval) + LRU program cache
//	bridge_shell.go   sandboxed subprocesses (allowlist via ShellOption)
//	bridge_fs.go      workspace files
//	bridge_runtime.go sub-script execScript (inherits parent bindings)
//	bridge_llm.go         NewLLMBridge facade + defaultable LLMRunOptions schema
//	bridge_llm_round.go   in-bridge round driver (resolver + GenerateStream + tool.Registry)
//	llm_marshal.go        model.* ⇄ map[string]any projections (multimodal-aware)
//	bridge_tools.go   tool.Registry (deny-by-default, explicit allowlist or AllowAll)
//	bridge_run.go     run metadata exposed from agent.RunInfo (run/task/agent/context ids)
//	*_test.go         table-driven / jsrt integration tests
//
// # Global naming convention
//
// Script global names match the name returned by BindingFunc: board, stream,
// expr, shell, fs, runtime (injected by scriptnode), run, tools, llm.
// Methods exposed by each bridge use lowercase_underscore style, consistent
// with existing script conventions.
//
// # Integration with hosts
//
//   - Graph ScriptNode: composes board+stream+expr+runtime in ExecuteBoard,
//     then appends shell/fs per node type (see graph/node/scriptnode).
//   - Engine / Agent: build the env directly with script.NewEnvBuilder or
//     script.BuildEnv and the bridges you need; add NewLLMBridge when LLM
//     access is required, and use NewRunInfoBridge(runInfo) to surface
//     run/task/agent/context ids. There is no global preset — the few lines of
//     EnvBuilder assembly are the preset.
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
