package workspace

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// This file is a deprecation shim. The execution-boundary types
// (CommandRunner, LocalCommandRunner, ScopedCommandRunner,
// NoopCommandRunner, ExecOptions, ExecResult, CommandOption,
// WithMaxOutput, NewLocalCommandRunner, NewScopedCommandRunner) all
// moved to sdk/sandbox in v0.2.0. The aliases and wrappers below keep
// existing imports compiling without behaviour change.
// Will be removed in v0.5.0 — same window as catalog.Deps.AgentTools
// and runner.WithActorKey.

// CommandRunner is an alias for sandbox.Runner.
//
// Deprecated: moved to sdk/sandbox.Runner.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
type CommandRunner = sandbox.Runner

// ExecOptions is an alias for sandbox.ExecOptions. The new type adds
// Env / Net / Resources policy fields; callers that used the legacy
// Env map[string]string knob should move to ExecOptions.Env.Inject.
//
// Deprecated: moved to sdk/sandbox.ExecOptions.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
type ExecOptions = sandbox.ExecOptions

// ExecResult is an alias for sandbox.ExecResult.
//
// Deprecated: moved to sdk/sandbox.ExecResult.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
type ExecResult = sandbox.ExecResult

// CommandOption is an alias for sandbox.Option.
//
// Deprecated: moved to sdk/sandbox.Option.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
type CommandOption = sandbox.Option

// LocalCommandRunner is an alias for sandbox.LocalRunner.
//
// Deprecated: moved to sdk/sandbox.LocalRunner.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
type LocalCommandRunner = sandbox.LocalRunner

// NoopCommandRunner is an alias for sandbox.NoopRunner.
//
// Deprecated: moved to sdk/sandbox.NoopRunner.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
type NoopCommandRunner = sandbox.NoopRunner

// WithMaxOutput is a thin wrapper around sandbox.WithMaxOutputBytes.
//
// Deprecated: moved to sdk/sandbox.WithMaxOutputBytes.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
func WithMaxOutput(n int64) CommandOption {
	return sandbox.WithMaxOutputBytes(n)
}

// NewLocalCommandRunner is a thin wrapper around sandbox.NewLocalRunner.
//
// Deprecated: moved to sdk/sandbox.NewLocalRunner.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
func NewLocalCommandRunner(rootDir string, opts ...CommandOption) *LocalCommandRunner {
	return sandbox.NewLocalRunner(rootDir, opts...)
}

// ScopedCommandRunner is the legacy struct-shaped whitelist runner. It
// is kept as a thin wrapper around sandbox.AllowCommands so existing
// callers continue to compile.
//
// Deprecated: replaced by the functional sandbox.AllowCommands(inner, allowed) decorator.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
type ScopedCommandRunner struct {
	inner sandbox.Runner
}

// Exec implements sandbox.Runner via the underlying AllowCommands
// decorator built in NewScopedCommandRunner.
//
// Deprecated: see ScopedCommandRunner.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
func (r *ScopedCommandRunner) Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error) {
	return r.inner.Exec(ctx, cmd, args, opts)
}

// NewScopedCommandRunner wraps inner with the given whitelist. Builds a
// sandbox.AllowCommands decorator under the hood.
//
// Deprecated: use sandbox.AllowCommands directly.
// Will be removed in v0.5.0 (same window as catalog.Deps.AgentTools and runner.WithActorKey).
func NewScopedCommandRunner(inner CommandRunner, allowed []string) *ScopedCommandRunner {
	return &ScopedCommandRunner{inner: sandbox.AllowCommands(inner, allowed)}
}
