package sandbox_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// captureRunner records the ExecOptions each Start receives so tests
// can observe how decorators rewrite per-call policy.
type captureRunner struct {
	opts []sandbox.ExecOptions
}

func (r *captureRunner) Close() error                       { return nil }
func (r *captureRunner) Capabilities() sandbox.Capabilities { return sandbox.Capabilities{} }
func (r *captureRunner) List(context.Context) ([]sandbox.SessionInfo, error) {
	return nil, nil
}
func (r *captureRunner) Terminate(context.Context, string) error { return nil }
func (r *captureRunner) Start(_ context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	r.opts = append(r.opts, spec.Opts)
	return nil, nil
}

func readOnly() *permission.Policy { return &permission.Policy{} }

// TestWithDefaultsPermissionMerge pins the security-biased merge rule: a
// caller that supplied a boundary keeps it, a pinned default applies when the
// caller supplied none, and defaults never widen a caller's decision.
func TestWithDefaultsPermissionMerge(t *testing.T) {
	t.Run("no caller, no default keeps the runner boundary", func(t *testing.T) {
		inner := &captureRunner{}
		runner := engine.WithDefaults(inner, sandbox.ExecOptions{})
		if _, err := runner.Start(context.Background(), sandbox.SessionSpec{}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got := inner.opts[0].Permission; got != nil {
			t.Fatalf("merged Permission = %+v, want nil (runner boundary)", got)
		}
	})

	t.Run("caller boundary is preserved", func(t *testing.T) {
		inner := &captureRunner{}
		runner := engine.WithDefaults(inner, sandbox.ExecOptions{})
		caller := readOnly()
		if _, err := runner.Start(context.Background(), sandbox.SessionSpec{
			Opts: sandbox.ExecOptions{Permission: caller},
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got := inner.opts[0].Permission; got != caller {
			t.Fatalf("merged Permission = %+v, want the caller's policy", got)
		}
	})

	t.Run("default applies when the caller supplies none", func(t *testing.T) {
		inner := &captureRunner{}
		def := readOnly()
		runner := engine.WithDefaults(inner, sandbox.ExecOptions{Permission: def})
		if _, err := runner.Start(context.Background(), sandbox.SessionSpec{}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got := inner.opts[0].Permission; got != def {
			t.Fatalf("merged Permission = %+v, want the configured default", got)
		}
	})

	t.Run("a default never widens a caller decision", func(t *testing.T) {
		inner := &captureRunner{}
		wide := &permission.Policy{Writes: permission.Writes{Paths: []string{filepath.Join(string(filepath.Separator), "wide")}}}
		runner := engine.WithDefaults(inner, sandbox.ExecOptions{Permission: wide})
		caller := readOnly()
		if _, err := runner.Start(context.Background(), sandbox.SessionSpec{
			Opts: sandbox.ExecOptions{Permission: caller},
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got := inner.opts[0].Permission; got != caller {
			t.Fatalf("merged Permission = %+v, want the caller's read-only policy", got)
		}
	})
}

// TestValidateExecPolicyPermission checks the boundary validation: what a
// backend cannot project is rejected rather than silently dropped, and the
// network half is refused because a nil Permission must mean "inherit" while a
// nil network means "unrestricted".
func TestValidateExecPolicyPermission(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "work")

	valid := []sandbox.ExecOptions{
		{},
		{Permission: &permission.Policy{}},
		{Permission: &permission.Policy{Writes: permission.Writes{Paths: []string{root}}}},
	}
	for i, opts := range valid {
		if err := engine.ValidateExecPolicy(opts); err != nil {
			t.Errorf("case %d rejected: %v", i, err)
		}
	}

	invalid := map[string]sandbox.ExecOptions{
		"relative path": {Permission: &permission.Policy{
			Writes: permission.Writes{Paths: []string{"work"}},
		}},
		"glob path": {Permission: &permission.Policy{
			Writes: permission.Writes{Paths: []string{root + string(filepath.Separator) + "*"}},
		}},
		"network on the permission": {Permission: &permission.Policy{
			Net: permission.NetPolicy{Mode: permission.NetDenyAll},
		}},
	}
	for name, opts := range invalid {
		if err := engine.ValidateExecPolicy(opts); err == nil {
			t.Errorf("%s accepted, want a validation error", name)
		}
	}
}
