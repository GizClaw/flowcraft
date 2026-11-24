package local

import (
	"context"
	"os/exec"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/sandbox/sessionkit"
)

// spawnProcess is the backend's [sandbox.SessionStarter]: it narrows the call's budgets against the
// confine, applies the confine's environment, and hands the command to the shared session layer.
//
// The unenforceable parts of a confine were refused at construction, so nothing here has to check a
// network posture or a write set: what is left is the part a call may ask for less of.
func (r *Runner) spawnProcess(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if len(spec.Argv) == 0 {
		return nil, errdefs.Validationf("sandbox: a session needs a command")
	}
	opts, err := r.confine.Narrow(spec.Opts)
	if err != nil {
		return nil, err
	}
	workDir, err := resolveWorkDir(opts.WorkDir)
	if err != nil {
		return nil, err
	}
	if opts.Resources.MaxOutputBytes <= 0 {
		opts.Resources.MaxOutputBytes = r.defaultMaxOutput
	}
	spec.Opts = opts

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = buildEnv(r.confine.Env)
	return sessionkit.StartSession(ctx, spec, cmd)
}
