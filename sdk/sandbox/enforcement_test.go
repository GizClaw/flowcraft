package sandbox_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// bareRunner implements Runner without the reporter interface, to pin
// the conservative-fallback contract of EnforcementOf.
type bareRunner struct{}

func (bareRunner) Exec(context.Context, string, []string, sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	return &sandbox.ExecResult{}, nil
}

func TestEnforcement_LocalRunner(t *testing.T) {
	e := sandbox.EnforcementOf(sandbox.NewLocalRunner(t.TempDir()))
	if !e.EnvAllowList {
		t.Error("LocalRunner must claim EnvAllowList")
	}
	if runtime.GOOS != "windows" {
		if !e.MemoryCap || !e.CPUCap {
			t.Errorf("unix LocalRunner must claim MemoryCap+CPUCap, got %+v", e)
		}
	} else if e.MemoryCap || e.CPUCap {
		t.Errorf("non-unix LocalRunner must not claim caps, got %+v", e)
	}
	if e.DiskCap {
		t.Error("DiskCap must stay unclaimed (no quota mechanism)")
	}
	if e.FilesystemBounds {
		t.Error("FilesystemBounds is OS-level confinement; LocalRunner must not claim it")
	}
	if len(e.NetModes) != 0 {
		t.Errorf("LocalRunner enforces no net mode, got %v", e.NetModes)
	}
}

func TestEnforcement_NoopRunner(t *testing.T) {
	if e := sandbox.EnforcementOf(sandbox.NoopRunner{}); !enforcementEqual(e, sandbox.Enforcement{}) {
		t.Errorf("NoopRunner must report zero enforcement, got %+v", e)
	}
}

func TestEnforcement_DecoratorsForwardInner(t *testing.T) {
	local := sandbox.NewLocalRunner(t.TempDir())
	want := sandbox.EnforcementOf(local)

	chained := sandbox.WithDefaults(
		sandbox.AllowCommands(local, []string{"echo"}),
		sandbox.ExecOptions{Timeout: 1},
	)
	if got := sandbox.EnforcementOf(chained); !enforcementEqual(got, want) {
		t.Errorf("decorated chain = %+v, want inner's %+v (decorators add no capability)", got, want)
	}

	// A decorator wrapped around a capability-free runner must not
	// invent capability either.
	noopChain := sandbox.WithDefaults(sandbox.NoopRunner{}, sandbox.ExecOptions{Timeout: 1})
	if got := sandbox.EnforcementOf(noopChain); !enforcementEqual(got, sandbox.Enforcement{}) {
		t.Errorf("decorated NoopRunner = %+v, want zero", got)
	}
}

func TestEnforcementOf_ConservativeFallback(t *testing.T) {
	if e := sandbox.EnforcementOf(nil); !enforcementEqual(e, sandbox.Enforcement{}) {
		t.Errorf("nil runner = %+v, want zero", e)
	}
	if e := sandbox.EnforcementOf(bareRunner{}); !enforcementEqual(e, sandbox.Enforcement{}) {
		t.Errorf("runner without reporter = %+v, want zero", e)
	}
}

func enforcementEqual(a, b sandbox.Enforcement) bool {
	if a.EnvAllowList != b.EnvAllowList || a.MemoryCap != b.MemoryCap ||
		a.CPUCap != b.CPUCap || a.DiskCap != b.DiskCap ||
		a.FilesystemBounds != b.FilesystemBounds {
		return false
	}
	if len(a.NetModes) != len(b.NetModes) {
		return false
	}
	for i := range a.NetModes {
		if a.NetModes[i] != b.NetModes[i] {
			return false
		}
	}
	return true
}
