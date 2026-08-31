//go:build windows

package windows

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"

	otellog "go.opentelemetry.io/otel/log"
)

// defaultMaxOutputBytes is the per-stream cap used by the one-shot
// Exec when ExecOptions.Resources.MaxOutputBytes is zero.
const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// Runner executes commands directly on the host via os/exec, with the
// child confined to a Windows Job Object that owns its whole process
// tree. It is the no-filesystem-isolation backend for Windows:
// lifecycle and resource caps are enforced by the kernel (job
// objects); filesystem and network confinement are phase 2.
//
// Policy support matrix:
//
//   - WorkDir / Stdin / Timeout: fully supported. Every child runs in
//     its own job; timeout/cancel closes the job and kills the whole
//     tree, not just the leader.
//   - TTY sessions: supported via ConPTY (merged SessionStreamTTY,
//     Resize). Signal stays NotAvailable. TTY combined with
//     WithWriteConfinement is NotAvailable until the restricted-token
//     spawn path is verified against a real pseudo console.
//   - Env: fully supported (see sandbox.EnvPolicy doc).
//   - Net.Mode: NetDefault (host networking) and NetDenyAll
//     (AppContainer without network capabilities) are supported;
//     NetAllowList / NetProxy need the WFP layer and are
//     errdefs.NotAvailable for now.
//   - Write == WriteReadOnly: errdefs.NotAvailable (no OS boundary to
//     confine writes yet).
//   - Resources.MemoryBytes: enforced as a job-wide memory limit
//     (JOB_OBJECT_LIMIT_JOB_MEMORY).
//   - Resources.CPUMillicores: enforced as a per-process user-time
//     limit = Timeout x millicores/1000; requires Timeout > 0,
//     otherwise errdefs.NotAvailable.
//   - Resources.DiskBytes != 0: errdefs.NotAvailable (no quota
//     mechanism).
//   - Resources.MaxOutputBytes: enforced; per-call value overrides the
//     runner's WithMaxOutputBytes default.
type Runner struct {
	rootDir      string
	cfg          runnerConfig
	sessions     *sandbox.SessionRegistry
	registryOnce sync.Once
	lowTemp      string
}

// New constructs a Runner rooted at rootDir. The root is resolved via
// filepath.Abs + EvalSymlinks so a later symlink swap on the root
// itself cannot be used to escape.
func New(rootDir string, opts ...Option) (*Runner, error) {
	real, err := filepath.Abs(rootDir)
	if err == nil {
		if resolved, evalErr := filepath.EvalSymlinks(real); evalErr == nil {
			real = resolved
		}
	}
	r := &Runner{
		rootDir: real,
		cfg:     runnerConfig{defaultMaxOutput: defaultMaxOutputBytes},
	}
	for _, o := range opts {
		o(&r.cfg)
	}
	if r.cfg.writeConfine {
		var err error
		r.cfg.writable, err = resolveAbsolutePaths(real, r.cfg.writable)
		if err != nil {
			return nil, err
		}
		r.lowTemp, err = createLowTempDir()
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Capabilities declares the honest surface: env allow-lists and
// job-object resource caps are always enforced; filesystem write
// bounds are enforced when the runner was constructed with
// WithWriteConfinement. TTY sessions are supported through ConPTY;
// signal and event features are not.
func (r *Runner) Capabilities() sandbox.Capabilities {
	policy := sandbox.Enforcement{
		EnvAllowList: true,
		MemoryCap:    true,
		CPUCap:       true,
	}
	if r.cfg.writeConfine {
		policy.FilesystemBounds = true
		policy.WriteModes = []sandbox.WritePolicy{
			sandbox.WriteWorkspace,
			sandbox.WriteReadOnly,
		}
	}
	policy.NetModes = []corenet.NetMode{corenet.NetDenyAll}
	return sandbox.Capabilities{
		Policy:   policy,
		Features: sandbox.SessionFeatures{TTY: true},
	}
}

// Exec runs cmd with args under opts. See Runner doc for which policy
// fields are honoured vs. rejected with errdefs.NotAvailable.
func (r *Runner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	return sandbox.Exec(ctx, r, cmd, args, opts)
}

// Start implements Runner: policy validation mirrors Exec, then the
// command is spawned on pipes inside a fresh job object.
func (r *Runner) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	return r.registry().Start(ctx, spec)
}

// List implements Runner.
func (r *Runner) List(ctx context.Context) ([]sandbox.SessionInfo, error) {
	return r.registry().List(ctx)
}

// Terminate implements Runner.
func (r *Runner) Terminate(ctx context.Context, id string) error {
	return r.registry().Terminate(ctx, id)
}

// Close implements core/sandbox.Runner: it terminates every session
// started through this runner. Safe to call more than once and when
// the runner never started anything.
func (r *Runner) Close() error {
	err := r.registry().Close()
	if r.lowTemp != "" {
		if rerr := os.RemoveAll(r.lowTemp); rerr != nil {
			telemetry.WarnErr(context.Background(),
				"windows: remove low-IL temp dir failed", rerr,
				otellog.String("windows.temp_dir", r.lowTemp))
		}
		r.lowTemp = ""
	}
	return err
}

// registry returns the session registry, initialising it lazily so a
// zero-value Runner still answers with NotAvailable instead of
// panicking on a nil interface.
func (r *Runner) registry() *sandbox.SessionRegistry {
	r.registryOnce.Do(func() {
		if r.sessions == nil {
			r.sessions = sandbox.NewSessionRegistry(r.spawnProcess)
		}
	})
	return r.sessions
}

// spawnProcess is Runner's sandbox.SessionStarter: it applies the
// same policy surface as Exec and hands the configured command to the
// session implementation, which owns job creation and assignment.
func (r *Runner) spawnProcess(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if err := r.validatePolicy(spec); err != nil {
		return nil, err
	}
	workDir, err := r.resolveWorkDir(spec.Opts.WorkDir)
	if err != nil {
		return nil, err
	}
	maxOut := spec.Opts.Resources.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = r.cfg.defaultMaxOutput
	}
	spec.Opts.Resources.MaxOutputBytes = maxOut

	env := buildEnv(spec.Opts.Env)
	var iso *netIsolation
	if spec.Opts.Net.Mode != corenet.NetDefault {
		if spec.TTY {
			return nil, errdefs.NotAvailablef(
				"windows: TTY sessions with a net policy are not supported yet")
		}
		writable := r.cfg.writable
		if spec.Opts.Write != sandbox.WriteReadOnly {
			writable = append([]string{r.rootDir}, writable...)
		}
		iso, err = newNetIsolation(r.rootDir, writable, spec.Opts.Net.Mode)
		if err != nil {
			return nil, err
		}
		// AppContainer cannot read the user profile; redirect the
		// child's home-facing env into the sandbox home.
		env = iso.env(env)
	}
	if spec.TTY {
		if r.cfg.writeConfine {
			return nil, errdefs.NotAvailablef(
				"windows: TTY sessions with write confinement are not supported yet")
		}
		env = sanitizeEnv(env)
		if err := ctx.Err(); err != nil {
			return nil, errdefs.FromContext(err)
		}
		return startTTYSession(ctx, spec, workDir, env)
	}

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = env
	if iso == nil && r.cfg.writeConfine {
		writable := r.cfg.writable
		if spec.Opts.Write != sandbox.WriteReadOnly {
			writable = append([]string{r.rootDir}, writable...)
		}
		for _, p := range writable {
			if err := labelLowIntegrity(p); err != nil {
				return nil, err
			}
		}
		cmd.Env = withLowTempEnv(cmd.Env, r.lowTemp)
	}
	cmd.Env = sanitizeEnv(cmd.Env)
	if err := ctx.Err(); err != nil {
		return nil, errdefs.FromContext(err)
	}
	return startSession(ctx, spec, cmd, r.cfg.writeConfine, iso)
}

// validatePolicy mirrors sandbox.ValidateExecPolicy minus the
// process-group sampling gate: the windows backend enforces Memory /
// CPU caps through job objects, so the shared unix ps(1) sampler
// availability check does not apply. The job-object caps themselves
// are enforced by the kernel, not sampled.
func (r *Runner) validatePolicy(spec sandbox.SessionSpec) error {
	if spec.Opts.Write > sandbox.WriteReadOnly {
		return errdefs.Validationf(
			"windows: unknown write policy %d", int(spec.Opts.Write))
	}
	if spec.Opts.Write == sandbox.WriteReadOnly && !r.cfg.writeConfine {
		return errdefs.NotAvailablef(
			"windows: write policy requires the runner to be constructed with WithWriteConfinement")
	}
	if spec.Opts.Net.Mode != corenet.NetDefault {
		switch spec.Opts.Net.Mode {
		case corenet.NetDenyAll:
			// Supported: AppContainer without network capabilities.
		default:
			return errdefs.NotAvailablef(
				"windows: net mode %d not supported yet", int(spec.Opts.Net.Mode))
		}
	}
	if spec.Opts.Resources.DiskBytes != 0 {
		return errdefs.NotAvailablef(
			"windows: disk limits not supported (no quota mechanism)")
	}
	if spec.Opts.Resources.CPUMillicores != 0 && spec.Opts.Timeout <= 0 {
		return errdefs.NotAvailablef(
			"windows: CPUMillicores requires a per-call Timeout to derive a cpu-time cap")
	}
	return nil
}

// buildEnv translates a sandbox.EnvPolicy into a flat []string
// suitable for exec.Cmd.Env. The empty result is returned as nil so
// os/exec falls back to its "no env at all" code path (which is what
// we want when the caller asked for an empty allow-list with no
// Inject).
func buildEnv(p sandbox.EnvPolicy) []string {
	var env []string

	if p.Allow == nil {
		env = append(env, os.Environ()...)
	} else if len(p.Allow) > 0 {
		allow := make(map[string]bool, len(p.Allow))
		for _, name := range p.Allow {
			allow[name] = true
		}
		for _, kv := range os.Environ() {
			idx := strings.IndexByte(kv, '=')
			if idx <= 0 {
				continue
			}
			if allow[kv[:idx]] {
				env = append(env, kv)
			}
		}
	}

	if len(p.Inject) > 0 {
		injected := make(map[string]bool, len(p.Inject))
		for k := range p.Inject {
			injected[k] = true
		}
		filtered := env[:0]
		for _, kv := range env {
			idx := strings.IndexByte(kv, '=')
			if idx <= 0 {
				filtered = append(filtered, kv)
				continue
			}
			if !injected[kv[:idx]] {
				filtered = append(filtered, kv)
			}
		}
		env = filtered
		for k, v := range p.Inject {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	if p.Allow != nil && len(p.Allow) == 0 && len(p.Inject) == 0 {
		// Distinguish "inherit nothing" from "inherit everything":
		// return an empty (non-nil) slice so exec.Cmd.Env is set to no
		// entries instead of falling back to os.Environ().
		return []string{}
	}
	return env
}

func (r *Runner) resolveWorkDir(dir string) (string, error) {
	abs := dir
	if !filepath.IsAbs(dir) {
		abs = filepath.Join(r.rootDir, dir)
	}
	abs = filepath.Clean(abs)

	real, err := sandbox.EvalExistingPrefix(abs)
	if err != nil {
		return "", fmt.Errorf("windows: resolve workdir: %w", err)
	}
	if !containedInRoot(real, r.rootDir) {
		return "", fmt.Errorf("%w: workdir %q escapes root", sandbox.ErrPathTraversal, dir)
	}
	return abs, nil
}

// containedInRoot reports whether path is root itself or directly
// under it. Windows paths are case-insensitive, so the prefix check
// folds case (this file only builds on Windows); the workspace layer's
// equivalent check does the same.
func containedInRoot(path, root string) bool {
	if strings.EqualFold(path, root) {
		return true
	}
	return strings.HasPrefix(strings.ToLower(path),
		strings.ToLower(root)+string(filepath.Separator))
}

// resolveAbsolutePaths makes each path absolute (relative entries are
// resolved against root) without requiring them to exist yet.
func resolveAbsolutePaths(root string, paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		out = append(out, filepath.Clean(p))
	}
	return out, nil
}

// sanitizeEnv drops entries containing NUL bytes. os/exec rejects such
// entries with a hard error, and a malformed entry in the host
// environment must not prevent spawning.
func sanitizeEnv(env []string) []string {
	out := env[:0]
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			continue
		}
		out = append(out, kv)
	}
	return out
}
