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
package config
