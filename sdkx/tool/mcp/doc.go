// Package mcp attaches Model Context Protocol servers to a
// [tool.Registry], turning every tool a server exposes into an ordinary
// [tool.Tool].
//
// # Why this is an attachment, not a tool
//
// MCP is not a built-in tool. The built-in set is fixed and small
// (sdkx/tool/{askuser,exec,kanban} plus agent.HandoffTool);
// everything else is delegated to the MCP ecosystem, which is what
// this package exists to reach. See sdkx/tool's package doc for the
// boundary between the two.
//
// The unit of integration is therefore a [Source]: a host-owned handle
// that connects to servers and writes their tools into the registry the
// host already has. There is no MCP-specific catalog and no
// MCP-specific dispatcher. Downstream consumers — the inference node
// resolving `tools: [...]` by name, the tool node dispatching a call,
// the script bridge's allow-list — see one registry containing both
// hand-written Go tools and MCP-provided ones, and cannot tell them
// apart. That is the point: execution policy (timeouts, approval,
// audit, rate limiting) is configured once as [tool.Executor]
// middleware and applies to MCP tools automatically, per tool name.
//
// # Wiring
//
//	reg := tool.NewRegistry()
//	reg.Register(askuser.New())          // built-in
//
//	src := mcp.NewSource(reg)
//	defer src.Close()
//
//	fs, err := mcp.Stdio("npx",
//	    []string{"-y", "@modelcontextprotocol/server-filesystem", root}, nil)
//	if err != nil { ... }
//	if err := src.AddServer(ctx, "filesystem", fs); err != nil { ... }
//
//	// reg now holds askuser plus filesystem__read_file, filesystem__write_file, ...
//	exec := tool.NewExecutor(reg, mws...)
//
// AddServer is synchronous: once it returns, the registry is complete
// for that server, so a host can finish wiring before serving traffic.
//
// # Namespacing
//
// Tool names are prefixed with "<serverName>__" by default, so two
// servers exposing a `read_file` register as `fs__read_file` and
// `docs__read_file` without collision. The separator is double
// underscore because it survives the `^[a-zA-Z0-9_-]+$` name
// restriction every major function-calling API enforces. [WithPrefix]
// overrides it; [WithScope] additionally controls registry visibility
// ([tool.ScopeAgent] to expose to models, [tool.ScopePlatform] to keep
// a server's tools callable but hidden from listings).
//
// # Discovery, caching, and refresh
//
// Discovery results are projected into the registry rather than fetched
// on demand. [tool.Catalog.Definitions] is synchronous, infallible, and
// consulted once per LLM turn; reaching out to every attached server on
// that path would let one slow server stall unrelated work.
//
// This package adds no cache of its own. The go-sdk maintains a TTL
// cache for tools/list — with the TTL advertised by the server — and
// invalidates it on a tools/list_changed notification. Source hooks
// that notification to reconcile the registry: new and changed tools are
// re-registered, vanished ones are unregistered, and other servers are
// untouched. [Source.Refresh] does the same on demand, which is what
// servers that never send the notification need.
//
// A tools/list failure leaves the previous projection in place. Making
// the model abruptly lose sight of tools it was told about is worse than
// letting the call fail with a clear per-server error.
//
// # Failure isolation
//
// Each server owns its session and its slice of the registry. When a
// server dies its tools return errdefs.NotAvailable naming that server;
// other servers and every built-in tool keep working. [Source.Close]
// releases all sessions, which for a stdio server means closing stdin,
// waiting, then escalating to SIGTERM and SIGKILL as the MCP spec
// prescribes — hosts never reap child processes themselves.
//
// # Result rendering
//
// [tool.Tool] returns one string, MCP returns a list of content parts,
// so the mapping is fixed and documented rather than left to chance:
//
//   - parts render in order, joined by "\n";
//   - a text part contributes its text verbatim;
//   - any other part (image, audio, resource link, embedded resource)
//     contributes its JSON wire form, so nothing is silently dropped;
//   - when there are no content parts but structuredContent is set,
//     the structured value is rendered as JSON instead.
//
// A result flagged isError becomes a non-nil error carrying the rendered
// content, which the executor reports as an error result — the model
// sees the failure and can self-correct. That is distinct from a
// transport failure, which is errdefs.NotAvailable and means the server
// itself is gone.
//
// # Metadata
//
// Adapted tools declare [tool.ToolMeta].SelfTimeout, since an MCP call
// is already bounded by the caller's context and the transport's own
// timeout; the timeout middleware's per-tool table still overrides
// that when a host wants a hard bound anyway.
//
// The MCP readOnlyHint annotation maps to
// [tool.ToolMeta].MutatesState. An absent annotation means "assume it
// mutates", matching both the MCP spec default and the conservative
// local default, so approval and retry policy err on the safe side for
// servers that declare nothing.
//
// # Secrets
//
// Per-server credentials go in the transport: environment entries for
// [Stdio], request headers for [StreamableHTTP]. Neither is logged.
package mcp
