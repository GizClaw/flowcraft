// Package config assembles a tool.Executor from a versioned YAML
// policy document. It mirrors sdkx/inference/config in spirit:
// strict decoding (unknown fields and kinds fail fast), a named
// factory catalog with no global registration, and application-side
// dependencies (Approver, AuditSink) injected at the Builder, never
// expressed in YAML.
//
// The document declares the middleware chain explicitly — array
// order is chain order, first entry outermost:
//
//	version: v1
//	middlewares:
//	  - kind: recover
//	  - kind: telemetry
//	  - kind: audit      # outer enough to observe approval denials too
//	  - kind: concurrency
//	    spec: { limit: 10 }
//	  - kind: timeout
//	    spec: { default: 30s, per_tool: { exec: 120s } }
//	  - kind: ratelimit
//	  - kind: approval
//	    spec: { tools: [exec, kanban_submit] }
//	scopes: { exec: platform }
//
// Built-in kinds: recover, telemetry, concurrency, timeout,
// ratelimit, approval, audit — each mapping to the same-named
// constructor in sdk/tool/middleware. Custom kinds register on the
// Builder via RegisterFactory. scopes applies registry-level scope
// metadata to already-registered tools.
//
// # Sources
//
// A document may also declare external tool providers, which attach
// before anything else so their tools are in the registry by the time
// scopes and middleware name them:
//
//	version: v1
//	sources:
//	  - kind: mcp
//	    spec:
//	      servers:
//	        - name: filesystem
//	          transport: stdio
//	          command: npx
//	          args: ["-y", "@modelcontextprotocol/server-filesystem", "/srv/data"]
//	middlewares:
//	  - kind: approval
//	    spec: { tools: [filesystem__write_file] }
//
// No source kind is registered by default, since each pulls in an
// external dependency; a host opts in explicitly:
//
//	builder.RegisterSourceFactory(mcp.SpecKind, mcp.SourceFactory)
//
// Sources are live resources — child processes, HTTP sessions — so
// Build returns an Assembly rather than a bare Executor, and the caller
// must Close it. A build that fails partway closes whatever already
// attached, so an error never leaves processes behind.
//
// Note what this does *not* introduce: a source writes into the same
// tool.Registry the Builder was given, so an externally-provided tool
// and a hand-written Go tool are the same kind of thing to every
// consumer downstream. There is one catalog and one dispatch path, and
// per-tool policy (approval, timeouts) addresses MCP tools by their
// namespaced names exactly as it addresses built-ins.
package config
