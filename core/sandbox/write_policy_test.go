package sandbox_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
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
	root := pathtest.Abs("work")
	caps := sandbox.Enforcement{MemoryCap: true, CPUCap: true}

	valid := []sandbox.ExecOptions{
		{},
		{Permission: &permission.Policy{}},
		{Permission: &permission.Policy{Writes: permission.Writes{Paths: []string{root}}}},
	}
	for i, opts := range valid {
		if err := engine.ValidateExecPolicy(opts, caps); err != nil {
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
		if err := engine.ValidateExecPolicy(opts, caps); err == nil {
			t.Errorf("%s accepted, want a validation error", name)
		}
	}
}

// TestValidateExecPolicyCaps pins the resource gate on the runner's own
// declaration: a cap the backend does not report is rejected instead of being
// silently dropped, and reporting it accepts the request. The shared layer no
// longer probes the platform for this, so the runner's Enforcement is the only
// source of truth.
func TestValidateExecPolicyCaps(t *testing.T) {
	none := sandbox.Enforcement{}
	full := sandbox.Enforcement{MemoryCap: true, CPUCap: true}

	for name, tc := range map[string]struct {
		opts    sandbox.ExecOptions
		caps    sandbox.Enforcement
		wantErr bool
	}{
		"memory without a cap": {
			opts:    sandbox.ExecOptions{Resources: sandbox.ResourceLimits{MemoryBytes: 1 << 20}},
			caps:    none,
			wantErr: true,
		},
		"memory with a cap": {
			opts: sandbox.ExecOptions{Resources: sandbox.ResourceLimits{MemoryBytes: 1 << 20}},
			caps: full,
		},
		"cpu without a cap": {
			opts: sandbox.ExecOptions{
				Timeout:   time.Second,
				Resources: sandbox.ResourceLimits{CPUMillicores: 500},
			},
			caps:    none,
			wantErr: true,
		},
		"cpu with a cap": {
			opts: sandbox.ExecOptions{
				Timeout:   time.Second,
				Resources: sandbox.ResourceLimits{CPUMillicores: 500},
			},
			caps: full,
		},
		"cpu without a timeout": {
			opts:    sandbox.ExecOptions{Resources: sandbox.ResourceLimits{CPUMillicores: 500}},
			caps:    full,
			wantErr: true,
		},
		"disk": {
			opts:    sandbox.ExecOptions{Resources: sandbox.ResourceLimits{DiskBytes: 1 << 20}},
			caps:    full,
			wantErr: true,
		},
	} {
		err := engine.ValidateExecPolicy(tc.opts, tc.caps)
		if tc.wantErr && err == nil {
			t.Errorf("%s: accepted, want a rejection", name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
		if tc.wantErr && !errdefs.IsNotAvailable(err) {
			t.Errorf("%s: want errdefs.NotAvailable, got %v", name, err)
		}
	}
}
