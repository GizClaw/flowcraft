package sandbox

import (
	"time"

	"github.com/GizClaw/flowcraft/core/policy"
)

// ExecOptions is one call: what to run, where, with what input, and how much of
// the instance's budget it may spend.
//
// It carries no permission. The egress posture and the environment allow-list are
// fixed when the backend instance is built — see [Confine] — because a backend
// serves exactly one session, so a per-call permission would only be a second
// channel for something already decided and verified. What a call may still do is
// ask for *less*: [policy.Resources] here is a narrowing of the instance's
// ceiling, and [Confine.Narrow] is the only place that resolves the two.
//
// Field semantics:
//
//   - WorkDir: the absolute directory the command runs in. It is required: the
//     platform has no notion of a session directory, and a backend with no path of
//     its own cannot invent a default. A backend that could read outside its
//     mounts would refuse the call anyway; a backend that cannot confine anything
//     must not pretend a default is a boundary.
//   - Stdin: bytes piped to the command's stdin. nil or empty means no stdin: the
//     child's fd 0 stays the OS default (/dev/null), so a program that branches on
//     stdin reads its operands instead of an empty pipe. An empty slice is not "a
//     pipe with nothing in it".
//   - Timeout: per-call deadline. Zero means "no sandbox-imposed timeout" (the
//     caller's ctx still applies). The ceiling for it lives in the grant, one
//     layer up: the sandbox applies what it is given.
//   - Resources: what to spend, at most the instance's ceiling. Zero fields
//     inherit it.
type ExecOptions struct {
	WorkDir   string
	Stdin     []byte
	Timeout   time.Duration
	Resources policy.Resources
}

// ExecResult captures the outcome of one command. ExitCode is the command's exit
// status (0 on success, non-zero on failure the OS surfaced). Stdout and Stderr
// are the captured output, truncated per stream to the call's
// Resources.MaxOutputBytes.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}
