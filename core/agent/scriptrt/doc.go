// Package scriptrt owns the deployment resource protocol for script
// runtimes. The contract itself is core/agent.ScriptRuntime; this
// package registers the concrete JavaScript and Lua implementations
// (scriptrt/jsrt and scriptrt/luart) as resource factories under the
// shared agent.ScriptRuntime kind.
package scriptrt
