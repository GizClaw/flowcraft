package remote_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/sandbox/local"
)

// newInner builds the backend the server runs commands on: the no-isolation runner, which is the
// one backend that needs nothing from the host. The wire protocol does not care which backend
// answers — that is exactly what these tests check — so a backend that confines something can be
// substituted here without touching them.
func newInner(tb testing.TB) sandbox.Runner {
	tb.Helper()
	runner, err := local.New(sandbox.Confine{})
	if err != nil {
		tb.Fatalf("local.New: %v", err)
	}
	tb.Cleanup(func() { _ = runner.Close() })
	return runner
}

// withWorkDir names the directory a session runs in. A backend owns no directory of its own — the
// caller names one per call — so every spec in these tests passes through here rather than repeating
// a temporary path at each call site.
func withWorkDir(t *testing.T, spec sandbox.SessionSpec) sandbox.SessionSpec {
	t.Helper()
	if spec.Opts.WorkDir == "" {
		spec.Opts.WorkDir = t.TempDir()
	}
	return spec
}
