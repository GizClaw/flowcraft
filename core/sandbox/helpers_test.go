package sandbox_test

import (
	"context"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// This helper was shared across the old single-package tests. The split
// keeps a copy here because a test file cannot span packages.

// captureRunner records the ExecOptions each Start receives, so a test can
// observe how a decorator rewrites a call.
type captureRunner struct {
	opts []sandbox.ExecOptions
}

func (r *captureRunner) Close() error                                        { return nil }
func (r *captureRunner) Capabilities() sandbox.Capabilities                  { return sandbox.Capabilities{} }
func (r *captureRunner) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }
func (r *captureRunner) Terminate(context.Context, string) error             { return nil }
func (r *captureRunner) Start(_ context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	r.opts = append(r.opts, spec.Opts)
	return nil, nil
}
