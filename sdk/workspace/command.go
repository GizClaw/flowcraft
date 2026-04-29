package workspace

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const defaultMaxCommandOutput int64 = 10 * 1024 * 1024

type CommandOption func(*LocalCommandRunner)

func WithMaxOutput(n int64) CommandOption {
	return func(r *LocalCommandRunner) {
		if n <= 0 {
			r.maxOutput = math.MaxInt64
		} else {
			r.maxOutput = n
		}
	}
}

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
	_, err := b.buf.Write(p[:space])
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// CommandRunner executes shell commands within or relative to a workspace.
type CommandRunner interface {
	Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error)
}

// ExecOptions configures a command execution.
type ExecOptions struct {
	WorkDir string
	Env     map[string]string
	Stdin   []byte
	Timeout time.Duration
}

// ExecResult captures command output.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// LocalCommandRunner executes commands on the local OS.
type LocalCommandRunner struct {
	rootDir   string
	maxOutput int64
}

func NewLocalCommandRunner(rootDir string, opts ...CommandOption) *LocalCommandRunner {
	real, err := filepath.Abs(rootDir)
	if err == nil {
		if resolved, evalErr := filepath.EvalSymlinks(real); evalErr == nil {
			real = resolved
		}
	}
	r := &LocalCommandRunner{rootDir: real, maxOutput: defaultMaxCommandOutput}
	for _, o := range opts {
		o(r)
	}
	return r
}

func (r *LocalCommandRunner) Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	c := exec.CommandContext(ctx, cmd, args...)
	c.Dir = r.rootDir
	if opts.WorkDir != "" {
		resolved, err := r.resolveWorkDir(opts.WorkDir)
		if err != nil {
			return nil, err
		}
		c.Dir = resolved
	}
	if len(opts.Env) > 0 {
		c.Env = os.Environ()
		for k, v := range opts.Env {
			c.Env = append(c.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}
	if opts.Stdin != nil {
		c.Stdin = bytes.NewReader(opts.Stdin)
	}

	var stdout, stderr limitedBuffer
	stdout.max = r.maxOutput
	stderr.max = r.maxOutput
	c.Stdout = &stdout
	c.Stderr = &stderr

	err := c.Run()
	result := &ExecResult{
		Stdout: stdout.buf.String(),
		Stderr: stderr.buf.String(),
	}
	if err != nil {
		if ctx.Err() != nil {
			return result, errdefs.FromContext(fmt.Errorf("workspace: exec %s: %w", cmd, ctx.Err()))
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("workspace: exec %s: %w", cmd, err)
	}
	return result, nil
}

func (r *LocalCommandRunner) resolveWorkDir(dir string) (string, error) {
	abs := dir
	if !filepath.IsAbs(dir) {
		abs = filepath.Join(r.rootDir, dir)
	}
	abs = filepath.Clean(abs)

	real, err := evalExistingPrefix(abs)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve workdir: %w", err)
	}
	if real != r.rootDir && !strings.HasPrefix(real, r.rootDir+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: workdir %q escapes root", ErrPathTraversal, dir)
	}
	return abs, nil
}

// NoopCommandRunner always returns an empty successful result.
type NoopCommandRunner struct{}

func (NoopCommandRunner) Exec(_ context.Context, _ string, _ []string, _ ExecOptions) (*ExecResult, error) {
	return &ExecResult{}, nil
}

// ScopedCommandRunner wraps a CommandRunner with a command whitelist.
// Only commands whose base name appears in the whitelist are permitted.
type ScopedCommandRunner struct {
	inner     CommandRunner
	whitelist map[string]bool
}

// NewScopedCommandRunner creates a runner that only allows listed commands.
func NewScopedCommandRunner(inner CommandRunner, allowed []string) *ScopedCommandRunner {
	wl := make(map[string]bool, len(allowed))
	for _, cmd := range allowed {
		wl[cmd] = true
	}
	return &ScopedCommandRunner{inner: inner, whitelist: wl}
}

func (r *ScopedCommandRunner) Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error) {
	if !r.whitelist[cmd] {
		return nil, fmt.Errorf("workspace: command %q is not in the whitelist", cmd)
	}
	return r.inner.Exec(ctx, cmd, args, opts)
}
