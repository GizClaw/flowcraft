// Package exec ships the LLM-callable "exec" tool: a generic shell
// command runner over [sandbox.Runner]. It is the missing piece that
// turns coding-agent style harnesses (SWE-bench, Terminal-Bench)
// into something the model can drive end-to-end — the model emits
// `exec(command, args...)`, the tool delegates to a sandboxed
// runner, and the captured stdout/stderr/exit_code surfaces back as
// the tool result.
//
// # Why sdkx
//
// sdk defines interfaces and primitives; sdkx ships concrete
// adapters. tool.Tool implementations are concrete adapters — they
// bridge the generic tool.Tool interface to one specific service —
// and therefore belong here, mirroring the existing
// sdk/llm → sdkx/llm/*, sdk/workspace → sdkx/tool/memory layouts.
// See sdkx/tool/history/doc.go for the same rationale applied to
// the history coordinator.
//
// # Deny-by-default
//
// [New] requires a non-nil [sandbox.Runner]. Callers cannot
// construct an exec tool that "falls back to host shell" by leaving
// the runner empty; that bypass would defeat the whole point of
// sdk/sandbox. The mistake is caught at wiring time
// (errdefs.Validation), not at the first LLM call. If you genuinely
// want a no-op for tests, pass [sandbox.NoopRunner]{} or build a
// runner with [sandbox.AllowCommands] around an empty whitelist.
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
// # Relationship to sandbox policy
//
// The tool itself carries zero policy; everything (env allow-list,
// network mode, resource caps, output truncation, command
// whitelist) lives in the injected [sandbox.Runner]. To restrict
// what the LLM can run, compose the runner before passing it in:
//
//	rn := sandbox.AllowCommands(
//	    sandbox.NewLocalRunner(workdir, sandbox.WithMaxOutputBytes(1<<20)),
//	    []string{"ls", "cat", "git", "go"},
//	)
//	t, err := exec.New(rn)
//
// The same sandbox can be shared with the script-engine shell
// bridge, vesseld Sandbox resources (planned, v0.2.0), and any
// future sdkx/sandbox/{nsjail,container,microvm} backend without
// changing this tool's call site.
package exec
