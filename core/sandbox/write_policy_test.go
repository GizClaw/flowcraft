package sandbox_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
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

// TestWithDefaultsWriteMerge pins the security-biased merge rule:
// either side being read-only wins, an unknown caller value is
// preserved for backend validation, and a read-only default can never
// be widened.
func TestWithDefaultsWriteMerge(t *testing.T) {
	tests := []struct {
		name     string
		caller   sandbox.WritePolicy
		def      sandbox.WritePolicy
		expected sandbox.WritePolicy
	}{
		{name: "defaults workspace", expected: sandbox.WriteWorkspace},
		{name: "caller narrows", caller: sandbox.WriteReadOnly, expected: sandbox.WriteReadOnly},
		{name: "default pins read-only", def: sandbox.WriteReadOnly, expected: sandbox.WriteReadOnly},
		{name: "both read-only", caller: sandbox.WriteReadOnly, def: sandbox.WriteReadOnly, expected: sandbox.WriteReadOnly},
		{name: "unknown caller preserved", caller: sandbox.WritePolicy(7), expected: sandbox.WritePolicy(7)},
		{name: "unknown caller cannot widen pinned default", caller: sandbox.WritePolicy(7), def: sandbox.WriteReadOnly, expected: sandbox.WriteReadOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &captureRunner{}
			runner := sandbox.WithDefaults(inner, sandbox.ExecOptions{Write: tt.def})
			if _, err := runner.Start(context.Background(), sandbox.SessionSpec{
				Opts: sandbox.ExecOptions{Write: tt.caller},
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if len(inner.opts) != 1 {
				t.Fatalf("captured %d calls, want 1", len(inner.opts))
			}
			if got := inner.opts[0].Write; got != tt.expected {
				t.Fatalf("merged Write = %d, want %d", int(got), int(tt.expected))
			}
		})
	}
}

// TestValidateExecPolicyWriteMode ensures the known write modes pass
// validation and unknown values fail closed instead of silently
// degrading to the runner default.
func TestValidateExecPolicyWriteMode(t *testing.T) {
	for _, mode := range []sandbox.WritePolicy{sandbox.WriteWorkspace, sandbox.WriteReadOnly} {
		if err := sandbox.ValidateExecPolicy(sandbox.ExecOptions{Write: mode}); err != nil {
			t.Fatalf("Write %d rejected: %v", int(mode), err)
		}
	}
	if err := sandbox.ValidateExecPolicy(sandbox.ExecOptions{Write: sandbox.WritePolicy(7)}); err == nil {
		t.Fatal("unknown write policy accepted")
	}
}
