// Package bindings exposes per-execution host APIs to script runtimes.
//
// # Architecture
//
// Scripts never reach the outer Go context directly; instead the host
// assembles an [agent.ScriptEnv] (built by [Assemble]) whose
// Bindings map provides named globals. Each binding is a
// [BindingFunc] returning (name, value), where value is usually a
// map[string]any of script-callable functions and scalars.
//
// Existing bridges:
//
//   - board: direct read/write view of the engine board, with
//     resolve/resolveString for ${board:<name>} interpolation
//
//   - expr: boolean expression evaluation with the same interpolation
//
//   - fs: scratch FS bridge (ls/read/write/replace/edit + glob/grep/
//     stat on the OS scratch bridge)
//
//   - host: the script-facing handle onto agent.Host — publish (to
//     arbitrary subjects), emit (per-node stream deltas via the
//     graph-provided emitter), checkInterrupt, askUser and reportUsage.
//     See NewHostBridge for the full method surface and identity
//     semantics
//
//   - shell: run shell commands against a scratch filesystem with
//     script-oriented output shape
//
//   - tools: script-callable facade over a tool.Dispatcher /
//     tool.Catalog pair with an allow-list — call for one shot,
//     callAll for an ordered batch that can forward model-issued
//     call ids, definitions for wire-ready tool declarations
//     (see NewToolBridge)
//
//   - inference: canonical inference operations — generate/embed/
//     transcribe resolve an explicit model through the
//     inference.Assembly, route variants defer target selection to a
//     route.Router and add a routing trace; stream/routeStream are the
//     pull-based iterator twins whose accumulated result matches the
//     unary shape, and transcribeSession/routeTranscribeSession open a
//     duplex session handle (send/next/result/interrupt/close). Requests
//     and responses are the canonical wire JSON, and multi-turn tool
//     loops are orchestrated in script-land (see NewInferenceBridge)
//
//   - runtime: named sub-script execution via runtime.execScript,
//     with nested-exec capability probing (see NewRuntimeBridge)
//
//   - run: read-only run identity (run_id / task_id / agent_id /
//     context_id / parent_run_id), sourced from the ambient RunInfo
//     in the context (see NewRunInfoBridge)
//
// # Providers
//
// A script host contributes bindings either through [NewBuilder], which
// adapts bridge funcs ([BindingFunc] / [LateBindingFunc]) to a
// [Provider], or by implementing [Provider] directly — the shape the
// graph engine wires as the "agent.ScriptBindings" deployment resource.
// A provider is built once and shared across runs, so per-execution
// state (the run's board, node identity, host, executing runtime and
// per-node emitter) arrives through [Invocation] on every Bind call.
// [Assemble] is the single assembly path: it runs the ordinary
// bindings, then an optional [LateProvider] phase that observes the
// environment built so far (the "runtime" global needs it, so nested
// sub-scripts inherit the final map), rejects a name bound twice, and
// rejects a name a script could not reference — every global must be an
// identifier that is not a JavaScript or Lua keyword. [Chain] composes
// providers, which is how a host extends the standard surface.
//
// The assembled surface is observable: [Assemble] sets a
// "script.bindings.count" attribute on the caller's span and, when
// debug logging is enabled, emits a "script bindings assembled" record
// carrying the node identity and the global names. The graph script
// node adds "script.bindings.source" ("engine" or "standard") to the
// node span, so a deployment can tell which surface an execution got.
//
// # Interpolation
//
// Config values that carry ${board:<path>} references are expanded by
// the graph kernel before the script node decodes, so scripts see
// resolved values in their config global. Scripts can also expand
// references dynamically through the board bridge's resolve (typed)
// and resolveString (text) helpers. References support dot paths
// (${board:user.name}), an optional default (${board:x:default}), and
// fail with a validation error when the referenced variable is missing
// and no default is given. Prefix a reference with a backslash
// (\${board:x}) to emit it literally.
//
// # Env shape
//
// The final agent.ScriptEnv carries:
//
//   - config: the raw node config map the script was given
//   - bindings: merged results of every registered binding, with the
//     ordinary-first / late-second order [Assemble] enforces
//
// Bindings intentionally share one board view so an expression can
// read what a previous script wrote, and write_copies are tracked so
// the script node can publish the updated board back after execution.
package bindings
