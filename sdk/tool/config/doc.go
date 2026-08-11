// Package config assembles a tool.Executor from a versioned policy
// document. It mirrors sdk/workspace/config in spirit: strict decoding
// (unknown fields and kinds fail fast), a named factory catalog with no
// global registration, and application-side dependencies (Approver,
// AuditSink) injected at the Builder, never expressed in the document.
//
// The document is JSON at the protocol level; YAML is accepted as
// authoring sugar through sdk/config/utils, which detects JSON by the
// Kubernetes rule (first non-whitespace byte is an open brace) and
// converts YAML before strict decoding:
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
//	    spec: { tools: [exec] }
//	scopes: { exec: platform }
//
// Built-in kinds: recover, telemetry, concurrency, timeout, ratelimit,
// approval, audit — each mapping to the same-named constructor in
// sdk/tool/middleware. Custom kinds register on the Builder via
// [Builder.RegisterFactory]. scopes applies registry-level scope
// metadata to tools in the built registry.
//
// # Sources
//
// A document may also declare external tool providers, which attach
// before anything else so their tools are in the registry by the time
// scopes and middleware name them. The builtin source kind is
// registered by default and resolves hand-written Go tools the host
// registered on the Builder — either pre-constructed values via
// RegisterBuiltin, or per-build factories via RegisterBuiltinFactory:
//
//	sources:
//	  - kind: builtin
//	    spec: {tools: [search, exec]}
//	  - kind: mcp
//	    spec: { ... }
//
// The document therefore lists every tool the agent can call — builtin
// and external alike — and Build creates the Registry it describes.
// External source kinds are opt-in through
// [Builder.RegisterSourceFactory], since each pulls in an external
// dependency.
//
// # Resource deps
//
// The tool.Assembly deployment resource declares one optional
// dependency, DepSandbox (type "sandbox.Runner"), so a deployment can
// bind a sandbox out of a sandbox.Registry:
//
//	resources:
//	  tools:
//	    kind: tool.Assembly
//	    impl: yaml
//	    deps: {sandbox: boxes/main}
//	    settings: {file: ./tools.yaml}
//
// The resolved value is carried in every source factory's Input.Deps.
// Builtin factories that build command tools (sdkx/tool/exec's
// sandbox-backed exec / exec_session) consume it there; source kinds
// that do not need it simply ignore the key.
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
// per-tool policy (approval, timeouts) addresses external tools by
// their namespaced names exactly as it addresses built-ins.
package config
