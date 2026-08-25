//go:build unix

package local

import (
	"context"
	"os/exec"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

// sessionsAvailable reports whether interactive process sessions can be
// started on this platform.
func sessionsAvailable() bool { return true }

// spawnProcess is Runner's sandbox.SessionStarter: it applies the
// same policy surface as Exec and hands the configured command to the
// shared session implementation.
func (r *Runner) spawnProcess(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if err := sandbox.ValidateExecPolicy(spec.Opts); err != nil {
		return nil, err
	}
	if spec.Opts.Net.Mode != corenet.NetDefault {
		return nil, errdefs.NotAvailablef(
			"sandbox: net policy not supported by local runner; requires a kernel-level isolation backend")
	}
	if spec.Opts.Write == sandbox.WriteReadOnly {
		return nil, errdefs.NotAvailablef(
			"sandbox: write policy not supported by local runner; requires a kernel-level isolation backend")
	}
	workDir, err := r.resolveWorkDir(spec.Opts.WorkDir)
	if err != nil {
		return nil, err
	}
	maxOut := spec.Opts.Resources.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = r.defaultMaxOutput
	}
	spec.Opts.Resources.MaxOutputBytes = maxOut

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = buildEnv(spec.Opts.Env)
	return sandbox.StartSession(ctx, spec, cmd)
}
