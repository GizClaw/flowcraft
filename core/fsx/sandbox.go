package fsx

import (
	"errors"

	"github.com/GizClaw/flowcraft/core/permission"
)

// ErrDenied is the classified failure of a policy check. Implementations
// wrap it so callers can test with errors.Is regardless of whether the
// refusal happened in-process or inside a supervising sandbox.
var ErrDenied = errors.New("fsx: access denied by policy")

// Sandbox is the per-call authorisation context. It is a value: build one
// per call, serialise it across a process boundary if the operation runs
// elsewhere, and never mutate a shared instance.
type Sandbox struct {
	// Policy is the boundary for this call. Nil keeps the implementation's
	// configured defaults; a supplied policy is the whole boundary for the
	// call, so an empty writable list means read-only. It is the same rule
	// sandbox.ExecOptions.Permission follows, which is what keeps the
	// in-process check and the kernel mounts from disagreeing.
	Policy *permission.Policy
	// Cwd is the executor-side working directory used to resolve
	// relative paths. It is executor-side by definition: a client must
	// not add local absolute paths to it.
	Cwd string
	// Roots are executor-side absolute directories the policy patterns
	// are relative to, outermost first.
	Roots []string
}

// Root returns the outermost root, or "" when none is configured.
func (s *Sandbox) Root() string {
	if s == nil || len(s.Roots) == 0 {
		return ""
	}
	return s.Roots[0]
}
