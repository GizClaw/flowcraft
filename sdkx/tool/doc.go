// Package tool is the home of FlowCraft's built-in tools, the
// declarative assembly layer that composes them, and the MCP bridge
// that reaches everything else.
//
// # The boundary: primitives in, catalog out
//
// FlowCraft ships a deliberately small set of tools. The rule that
// keeps it small:
//
//	Built-in tools are orchestration primitives — capabilities that are
//	part of the engine's own contract and cannot be supplied by an
//	external, out-of-process provider. Every other capability is
//	delegated to the Model Context Protocol ecosystem rather than
//	hand-written and maintained here.
//
// The reason is maintenance arithmetic, not minimalism for its own
// sake. Every shipped tool is a permanent liability: upstream API
// drift, auth models, security review. A large built-in catalog also
// dilutes what this library is — an orchestration engine, not a tool
// directory. Since FlowCraft is an MCP client (see the mcp
// subpackage), "delegate to MCP" is a real, supported path: thousands
// of maintained servers supply domain capabilities at zero cost to
// this codebase.
//
// The operative test for a proposed tool is whether an out-of-process
// server could provide it. If it could, it is delegated. A tool stays
// built-in only when it reaches something a separate process cannot:
// the in-process agent.Host, the sandbox boundary, or live
// orchestration state. Each built-in package's doc.go names which of
// those it reaches.
//
// # Built-in tools
//
// Three packages. Each declares its primitive category in its own doc.go:
//
//	package      tool name(s)                 category
//	------------ ---------------------------- --------------------------
//	askuser      ask_user                     host bridge
//	exec         exec                         sandbox boundary
//	delegation   delegate, delegation_status  orchestration state
//
// The delegation tools adapt the backend-neutral sdk/delegation contracts.
// They discover targets through a Directory and recover a Service from the
// execution Host; asynchronous callers see only delegation_id, never backend
// card identifiers or operational storage details.
//
// # Everything else: MCP
//
// Web search, generic HTTP, cloud and SaaS APIs (GitHub, Slack, Jira,
// …), databases, generic filesystem access, third-party services —
// none of these ship here. Attach an MCP server instead:
//
//	src := mcp.NewSource(reg)
//	defer src.Close()
//	transport, _ := mcp.Stdio("npx",
//	    []string{"-y", "@modelcontextprotocol/server-github"}, env)
//	src.AddServer(ctx, "github", transport)
//
// The server's tools land in the same tool.Registry as the built-ins
// and are indistinguishable from them at every call site. See the mcp
// subpackage for transports, namespacing, and lifecycle, and the
// config subpackage for declaring servers in YAML.
//
// # Assembly
//
// The config subpackage turns a versioned YAML document into a
// tool.Executor: the middleware chain (recover, telemetry,
// concurrency, timeout, rate limit, approval, audit) plus any external
// sources to attach. Built-in tools are registered in Go by the host;
// config governs execution policy and external attachments, not which
// primitives exist.
package tool
