//go:build linux

package nsjail

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// Runner is an nsjail-backed sandbox.Runner. It is only constructible
// on Linux; see [New] for non-Linux behaviour.
type Runner struct {
	rootDir          string
	binary           string
	extraFlags       []string
	defaultMaxOutput int64
}

// New returns a Runner that confines child processes with nsjail.
// rootDir bounds WorkDir resolution exactly as it does for
// sandbox.LocalRunner.
//
// Errors:
//   - errdefs.NotAvailable when the nsjail binary cannot be found
//     (caller can fall back to LocalRunner or refuse to start).
//   - errdefs.Validation when rootDir cannot be resolved.
//
// The returned Runner is safe for concurrent use.
func New(rootDir string, opts ...RunnerOption) (*Runner, error) {
	cfg := &runnerConfig{}
	for _, o := range opts {
		o(cfg)
	}

	binary := cfg.binFrom
	if binary == "" {
		resolved, err := exec.LookPath("nsjail")
		if err != nil {
			return nil, errdefs.NotAvailablef(
				"nsjail: binary not found on PATH; install nsjail or use WithBinary")
		}
		binary = resolved
	} else if _, err := exec.LookPath(binary); err != nil {
		// Validate up front so a misconfigured path surfaces at
		// construction time, not at the first Exec.
		return nil, errdefs.NotAvailablef(
			"nsjail: binary %q not executable: %v", binary, err)
	}

	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, errdefs.Validationf("nsjail: resolve rootDir: %v", err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = resolved
	}

	return &Runner{
		rootDir:          abs,
		binary:           binary,
		extraFlags:       append([]string(nil), cfg.extra...),
		defaultMaxOutput: defaultMaxOutputBytes,
	}, nil
}

// Exec runs cmd with args inside an nsjail invocation that enforces
// opts.Net / opts.Resources at the kernel level. The function never
// downgrades policy: opts the backend cannot honour cause
// errdefs.NotAvailable, never a silent best-effort run.
func (r *Runner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	if cmd == "" {
		return nil, errdefs.Validationf("nsjail: empty command")
	}

	resolvedWorkDir, err := r.resolveWorkDir(opts.WorkDir)
	if err != nil {
		return nil, err
	}
	// Push the resolved WorkDir back so nsjail receives an absolute,
	// vetted path rather than the caller's raw input.
	opts.WorkDir = resolvedWorkDir

	flags, err := buildFlags(opts, os.Environ())
	if err != nil {
		return nil, err
	}
	flags = append(flags, r.extraFlags...)

	// nsjail also enforces opts.Timeout via --time_limit, but we keep
	// a Go-side ctx deadline as a belt-and-braces fallback: nsjail's
	// timer has whole-second resolution, and a hung nsjail process
	// itself would otherwise be invisible to ctx.Done().
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	argv := append([]string{}, flags...)
	argv = append(argv, "--", cmd)
	argv = append(argv, args...)

	c := exec.CommandContext(ctx, r.binary, argv...)
	// nsjail itself runs in the host env. The child's env is shaped
	// by --env / --keep_env flags inside buildFlags.
	c.Env = os.Environ()

	if opts.Stdin != nil {
		c.Stdin = bytes.NewReader(opts.Stdin)
	}

	maxOut := opts.Resources.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = r.defaultMaxOutput
	}
	if maxOut <= 0 {
		maxOut = math.MaxInt64
	}
	var stdout, stderr limitedBuffer
	stdout.max = maxOut
	stderr.max = maxOut
	c.Stdout = &stdout
	c.Stderr = &stderr

	runErr := c.Run()
	result := &sandbox.ExecResult{
		Stdout: stdout.buf.String(),
		Stderr: stderr.buf.String(),
	}
	if runErr != nil {
		if ctx.Err() != nil {
			return result, errdefs.FromContext(fmt.Errorf("nsjail: exec %s: %w", cmd, ctx.Err()))
		}
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("nsjail: exec %s: %w", cmd, runErr)
	}
	return result, nil
}

// resolveWorkDir applies the same root-confinement rules LocalRunner
// uses. Empty WorkDir resolves to the runner's root; relative paths
// are joined onto the root; absolute paths must stay inside it.
func (r *Runner) resolveWorkDir(dir string) (string, error) {
	if dir == "" {
		return r.rootDir, nil
	}
	abs := dir
	if !filepath.IsAbs(dir) {
		abs = filepath.Join(r.rootDir, dir)
	}
	abs = filepath.Clean(abs)

	real, err := evalExistingPrefix(abs)
	if err != nil {
		return "", fmt.Errorf("nsjail: resolve workdir: %w", err)
	}
	if real != r.rootDir && !strings.HasPrefix(real, r.rootDir+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: workdir %q escapes root", sandbox.ErrPathTraversal, dir)
	}
	return abs, nil
}

func evalExistingPrefix(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	realParent, err := evalExistingPrefix(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(path)), nil
}

// limitedBuffer mirrors sandbox.limitedBuffer (unexported there): a
// bytes.Buffer that silently drops writes past max. We duplicate the
// few lines instead of exposing the type because the contract is
// internal to the runner.
type limitedBuffer struct {
	buf bytes.Buffer
	max int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.max <= 0 || int64(b.buf.Len()) >= b.max {
		return len(p), nil
	}
	space := b.max - int64(b.buf.Len())
	if int64(len(p)) <= space {
		return b.buf.Write(p)
	}
	if _, err := b.buf.Write(p[:space]); err != nil {
		return 0, err
	}
	return len(p), nil
}
