// Package mcp attaches Model Context Protocol servers to a
// [tool.Registry], turning every tool a server exposes into an ordinary
// [tool.Tool].
//
// # Why this is an attachment, not a tool
//
// MCP is not a built-in tool. core/tool holds no concrete tool at all:
// the built-in set is the one the runtime wires from the configuration,
// core/tool-builtin/files, core/tool-builtin/exec and
// core/tool-builtin/skills, plus the delegation tools core/delegation
// injects into a runtime. Everything else is delegated to the MCP
// ecosystem, which is what this package exists to reach. See core/tool's
// package doc for the boundary between the two.
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
// The source is created empty and the registry is built over it, so the
// tools a server publishes arrive in a catalog that already exists:
//
//	src := mcp.NewSource()
//	defer src.Close()
//
//	reg, err := tool.NewRegistry([]tool.Source{builtin, src})
//	if err != nil { ... }
//	defer reg.Close()
//
//	fs, err := mcp.Stdio("npx",
//	    []string{"-y", "@modelcontextprotocol/server-filesystem", root}, nil)
//	if err != nil { ... }
//	if err := src.AddServer(ctx, "filesystem", fs); err != nil { ... }
//
//	// reg now holds the built-in tools plus filesystem__read_file,
//	// filesystem__write_file, ...
//	exec := tool.NewExecutor(reg, mws...)
//
// The defers run in reverse order, so the registry closes first and
// detaches the source before the servers shut down. A catalog that
// outlived its servers would keep handing out tools nothing can serve.
//
// A deployment does not call AddServer at all: it hands this package the
// `tools.mcp` block, and the runtime picks one of two constructors.
// [Connect] starts every declared server at assembly; [NewLazy] publishes
// one placeholder per server and connects it only when a tool from that
// server is loaded, so a host with ten servers does not pay ten processes
// for tools the model never asks for.
//
// AddServer never blocks on the network: it validates the
// configuration, registers the server, and schedules the first
// connection attempt to run immediately in the background. A server
// that cannot be reached is not a startup failure — the Source keeps
// retrying with exponential backoff and publishes the server's tools
// to the registry the moment a connection succeeds — and a hung server
// cannot stall the host past a background attempt. A server that cannot
// be started at all — a command that is missing, not executable, or not
// a runnable file — or one that rejects the connection is given up
// instead, so a permanently broken declaration does not churn against
// the environment forever; [WithServerLost] reports that give-up to a
// host that wants to know. Hosts that need a server up before
// proceeding mark it required and call [Source.WaitReady]; everything
// else converges while the host keeps starting.
//
// # Namespacing
//
// Tool names are prefixed with "<serverName>__" by default, so two
// servers exposing a `read_file` register as `fs__read_file` and
// `docs__read_file` without collision. The separator is double
// underscore because it survives the `^[a-zA-Z0-9_-]+$` name
// restriction every major function-calling API enforces. [WithPrefix]
// overrides it for one server. Visibility is not this package's
// decision: an MCP tool is an ordinary [tool.Tool], so whether the model
// sees it every turn, only after a search, or never is the dynamic
// exposure policy of core/tool/engine.
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
// invalidates it on a tools/list_changed notification. The Source hooks
// that notification to reconcile the registry: new and
// changed tools are added, vanished ones are removed, and other
// servers are untouched. The same reconcile runs when a background
// reconnect brings a server back, which is what servers that were down
// at startup need.
//
// A tools/list failure leaves the previous projection in place. Making
// the model abruptly lose sight of tools it was told about is worse than
// letting the call fail with a clear per-server error.
//
// # Failure isolation
//
// Each server owns its session and its slice of the registry. When a
// server dies its tools return errdefs.NotAvailable naming that server;
// other servers and every built-in tool keep working. The Source also
// reconnects a dead server in the background, and its tools are
// re-published when it comes back. [Source.Close] cancels all
// reconnection and releases every session, which for a stdio server
// means closing stdin, waiting, then escalating to SIGTERM and SIGKILL
// as the MCP spec prescribes — hosts never reap child processes
// themselves.
//
// # Result mapping
//
// [tool.Tool] returns [message.Content] and MCP returns a list of
// content parts, so the mapping is structural rather than textual:
//
//   - text, image, and audio content become their typed parts;
//   - a resource link becomes a [message.FilePart];
//   - an embedded resource contributes its text or its typed media
//     payload;
//   - anything with no typed home — including content types this
//     client does not know yet — keeps its JSON wire form as a
//     [message.DataPart] (or a text part when it is not a JSON
//     object), so nothing is silently dropped;
//   - when there are no content parts but structuredContent is set,
//     the structured value is carried as a data part instead.
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
