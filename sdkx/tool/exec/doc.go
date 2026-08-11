// Package exec ships the LLM-callable command tools: "exec", a
// one-shot shell command runner over [sandbox.Runner], and
// "exec_session", an interactive / streaming session driver over
// [sandbox.ProcessManager]. Together they turn coding-agent style
// harnesses (SWE-bench, Terminal-Bench) into something the model can
// drive end-to-end — the model emits `exec(command, args...)` or
// `exec_session(action, session_id, ...)`, the tool delegates to a
// sandboxed runner, and output surfaces back as the tool result.
//
// # Primitive category: sandbox boundary
//
// These tools ship built-in because they *are* the execution boundary.
// An out-of-process MCP server cannot supply them: the point of exec
// and exec_session is to run a command under the host's own
// [sandbox.Runner] / [sandbox.ProcessManager], honouring that runner's
// isolation, approval, and resource policy. A remote server would run
// commands in its own environment under its own rules, which is a
// different capability wearing the same name. See sdkx/tool's package
// doc for the boundary this sits on; hosts wanting command execution
// *somewhere else* should attach an MCP server for that environment
// instead.
//
// # Why sdkx
//
// sdk defines interfaces and primitives; sdkx ships concrete
// adapters. tool.Tool implementations are concrete adapters — they
// bridge the generic tool.Tool interface to one specific service —
// and therefore belong here, mirroring the existing
// sdk/inference → sdkx/inference/* layout.
//
// # Deny-by-default
//
// [New] requires a non-nil [sandbox.Runner] and [NewSession] requires
// a non-nil [sandbox.ProcessManager]. Callers cannot construct a tool
// that "falls back to host shell" by leaving the runner empty; that
// bypass would defeat the whole point of sdk/sandbox. The mistake is
// caught at wiring time (errdefs.Validation), not at the first LLM
// call. If you genuinely want a no-op for tests, pass
// [sandbox.NoopRunner]{} or build a runner with
// [sandbox.AllowCommands] around an empty whitelist.
//
// # Wire shape
//
// Arguments (JSON object):
//
//	{
//	  "command":         "string, the program to run (required)",
//	  "args":            ["string", ...],
//	  "workdir":         "string, relative to the sandbox root",
//	  "stdin":           "string, bytes piped to the program stdin",
//	  "timeout_seconds": 30
//	}
//
// Result (JSON object, returned as the tool result string):
//
//	{
//	  "exit_code": 0,
//	  "stdout":    "...",
//	  "stderr":    "..."
//	}
//
// A non-zero exit code is NOT a tool error — it is reported via
// exit_code so the LLM can reason about it. The tool only returns
// an errdefs-categorised Go error when the call cannot be made at
// all (validation failure, sandbox policy rejection, timeout, IO
// error from the runner).
//
// # exec_session wire shape
//
// The session tool keeps a process alive across calls and replays its
// output with a sequence cursor:
//
//	{
//	  "action":          "start|read|write|resize|status|signal|terminate|close",
//	  "session_id":      "session id from start (all actions except start)",
//	  "command":         "program to run (start)",
//	  "args":            ["arg", ...],
//	  "workdir":         "relative to the sandbox root",
//	  "tty":             false,
//	  "rows":            24,
//	  "cols":            80,
//	  "timeout_seconds": 300,
//	  "after_seq":       0,
//	  "max_bytes":       4096,
//	  "data":            "input to write",
//	  "signal":          "interrupt (signal action)"
//	}
//
// read returns {"next_seq", "eof", "chunks":[{"seq","stream","data"}]}
// with stream "stdout", "stderr", or "tty" (merged). status returns
// {"running", "exit_code", "reason", "signal", "pid", "tty", "argv"}.
// Policy is fixed once at start; an interactive session is an
// all-or-nothing command channel. Idle sessions are reclaimed by a
// TTL (default 30 minutes, configurable via [WithSessionTTL]) and all
// sessions are terminated by [SessionTool.Close].
//
// # Relationship to sandbox policy
//
// The tool itself carries zero policy; everything (env allow-list,
// network mode, resource caps, output truncation, command whitelist,
// approval) lives in the injected runner. To restrict what the LLM can
// run, compose the runner before passing it in:
//
//	rn := sandbox.AllowCommands(
//	    sandbox.NewLocalRunner(workdir, sandbox.WithMaxOutputBytes(1<<20)),
//	    []string{"ls", "cat", "git", "go"},
//	)
//	t, err := exec.New(rn)
//
// The decorators forward [sandbox.ProcessManager] too, so the same
// composed chain powers exec_session:
//
//	pm := sandbox.ProcessManagerOf(rn)
//	st, err := exec.NewSession(pm)
//
// The same sandbox can be shared with the script-engine shell bridge,
// host-application sandbox resources, and the sdkx/sandbox/{seatbelt,
// bwrap} backends without changing these tools' call sites.
//
// # Declarative wiring
//
// Hosts that assemble through sdk/tool/config can register both tools
// as builtin factories with [RegisterBuiltin]. Each factory is invoked
// once per assembly and reads the sandbox runner out of the
// tool.Assembly resource's `sandbox` dep, so the deployment document
// chooses which sandbox the tools execute under instead of the host
// hard-wiring one:
//
//	exec.RegisterBuiltin(toolBuilder)
//
//	resources:
//	  tools:
//	    kind: tool.Assembly
//	    impl: yaml
//	    deps: {sandbox: boxes/coding}
//	    settings: {file: ./tools.yaml}
//
//	tools.yaml:
//	  sources:
//	    - kind: builtin
//	      spec: {tools: [exec, exec_session]}
//
// The exec factory fails closed when the dep is missing or not a
// sandbox.Runner; exec_session additionally fails with NotAvailable
// when the runner has no ProcessManager.
package exec
