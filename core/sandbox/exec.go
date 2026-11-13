package sandbox

import (
	"time"

	"github.com/GizClaw/flowcraft/core/permission"
)

// ExecOptions configures one Runner.Exec call.
//
// Field semantics:
//
//   - WorkDir: directory the command runs in. Relative paths are resolved
//     against the runner's root (e.g. local.Runner.rootDir); absolute paths
//     must stay inside the root or the call is rejected with
//     ErrPathTraversal. Empty means "use the runner's root".
//   - Stdin: bytes piped to the command's stdin. nil or empty means no
//     stdin: the child's fd 0 stays the OS default (/dev/null), so a
//     program that branches on stdin reads its operands instead of an
//     empty pipe. An empty slice is not "a pipe with nothing in it".
//   - Timeout: per-call deadline. Zero means "no sandbox-imposed timeout"
//     (the caller's ctx still applies).
//   - Env: see EnvPolicy. Replaces the historical "inherit everything"
//     behaviour while staying back-compat when EnvPolicy.Allow is nil.
//   - Net: see NetPolicy. The local runner only accepts NetDefault.
//   - Permission: the file boundary for this call. Nil keeps the runner's
//     construction-time boundary (root plus explicit writable paths); a
//     supplied policy replaces it, so an empty writable list means
//     read-only for that call. There is deliberately no wider value: a
//     call can narrow the boundary, never widen it.
//   - Resources: see ResourceLimits. The local runner enforces MemoryBytes,
//     CPUMillicores (with Timeout), and MaxOutputBytes; DiskBytes is
//     still errdefs.NotAvailable.
type ExecOptions struct {
	WorkDir    string
	Stdin      []byte
	Timeout    time.Duration
	Env        EnvPolicy
	Net        permission.NetPolicy
	Permission *permission.Policy
	Resources  ResourceLimits
}

// Writable returns the absolute paths this call may write, given the runner's
// construction-time defaults.
//
// A supplied Permission replaces the defaults rather than adding to them: the
// caller stated the boundary, so the backend must not quietly widen it back to
// the runner root. An empty writable list therefore means read-only, which is
// how a call asks for it without needing a second enum.
func (o ExecOptions) Writable(defaults []string) []string {
	if o.Permission != nil {
		return o.Permission.Writes.Writable()
	}
	return defaults
}

// ExecResult captures the outcome of a Runner.Exec call. ExitCode is the
// command's exit status (0 on success, non-zero on failure that the OS
// surfaced via *exec.ExitError or equivalent). Stdout / Stderr are the
// captured output, possibly truncated to Resources.MaxOutputBytes.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}
