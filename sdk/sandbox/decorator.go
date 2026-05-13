package sandbox

import (
	"context"
	"fmt"
)

// AllowCommands returns a Runner that delegates to inner only when the
// command's exact name appears in allowed; every other command is
// rejected before reaching inner. It is the functional replacement for
// the v0.1 ScopedCommandRunner type — a decorator rather than a struct
// with exported fields. Matching is on the full command string passed to
// Exec; callers that want to match base names (so "/usr/bin/echo"
// matches "echo") should normalise before invoking Exec.
func AllowCommands(inner Runner, allowed []string) Runner {
	wl := make(map[string]bool, len(allowed))
	for _, cmd := range allowed {
		wl[cmd] = true
	}
	return &allowCommandsRunner{inner: inner, whitelist: wl}
}

type allowCommandsRunner struct {
	inner     Runner
	whitelist map[string]bool
}

func (r *allowCommandsRunner) Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error) {
	if !r.whitelist[cmd] {
		return nil, fmt.Errorf("sandbox: command %q is not in the whitelist", cmd)
	}
	return r.inner.Exec(ctx, cmd, args, opts)
}

// NoopRunner is a zero-policy Runner that always returns an empty
// successful ExecResult. It is useful as a default in test wiring or as
// the inner Runner for AllowCommands when the caller wants to assert
// "the allow-list rejected the call" without actually running anything.
type NoopRunner struct{}

// Exec implements Runner. It ignores every argument and returns an empty
// ExecResult with nil error.
func (NoopRunner) Exec(_ context.Context, _ string, _ []string, _ ExecOptions) (*ExecResult, error) {
	return &ExecResult{}, nil
}
