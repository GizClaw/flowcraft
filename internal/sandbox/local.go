package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// LocalSandbox runs commands as local OS processes scoped to a directory.
// On Linux with bwrap available, commands are isolated via Bubblewrap namespaces.
// Otherwise falls back to bare process execution with path validation.
type LocalSandbox struct {
	id               string
	rootDir          string
	closed           atomic.Bool
	readOnlyTargets  []string    // read-only symlink targets (skills/ etc.)
	readWriteTargets []string    // read-write symlink targets (data/ etc.)
	isolation        probeResult // determined at creation time
	bwrapCfg         BwrapConfig
}

// LocalOption configures a LocalSandbox during creation.
type LocalOption func(*LocalSandbox)

// SymlinkSpec describes a symbolic link and its read/write property.
type SymlinkSpec struct {
	Name     string // link name inside sandbox (e.g. "skills")
	Target   string // host real path
	ReadOnly bool   // true = read-only bind in bwrap, false = read-write
}

// WithSymlinks creates symbolic links inside the sandbox rootDir pointing to
// external directories (e.g. Workspace skills/ and data/). The symlink targets
// are added to read-only or read-write allow-lists so resolvePath permits access.
func WithSymlinks(specs []SymlinkSpec) LocalOption {
	return func(sb *LocalSandbox) {
		for _, spec := range specs {
			linkPath := filepath.Join(sb.rootDir, spec.Name)
			if existing, err := os.Readlink(linkPath); err == nil && existing == spec.Target {
				realTarget, _ := filepath.EvalSymlinks(spec.Target)
				if realTarget == "" {
					realTarget = spec.Target
				}
				sb.addTarget(realTarget, spec.ReadOnly)
				continue
			}
			_ = os.Remove(linkPath)
			if err := os.Symlink(spec.Target, linkPath); err == nil {
				realTarget, _ := filepath.EvalSymlinks(spec.Target)
				if realTarget == "" {
					realTarget = spec.Target
				}
				sb.addTarget(realTarget, spec.ReadOnly)
			}
		}
	}
}

func (s *LocalSandbox) addTarget(realTarget string, readOnly bool) {
	if readOnly {
		s.readOnlyTargets = append(s.readOnlyTargets, realTarget)
	} else {
		s.readWriteTargets = append(s.readWriteTargets, realTarget)
	}
}

// WithBwrapConfig sets Bubblewrap-specific configuration.
func WithBwrapConfig(cfg BwrapConfig) LocalOption {
	return func(sb *LocalSandbox) {
		sb.bwrapCfg = cfg
	}
}

// WithIsolation injects a pre-probed isolation result, avoiding repeated smoke tests.
// Manager caches the probe at init and injects via this option.
func WithIsolation(pr probeResult) LocalOption {
	return func(sb *LocalSandbox) {
		sb.isolation = pr
	}
}

// NewLocalSandbox creates a sandbox backed by a local directory.
func NewLocalSandbox(id, rootDir string, opts ...LocalOption) (*LocalSandbox, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("sandbox: create root dir: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("sandbox: eval symlinks on root: %w", err)
	}
	sb := &LocalSandbox{id: id, rootDir: real}
	for _, opt := range opts {
		opt(sb)
	}
	if sb.isolation == (probeResult{}) {
		pr, err := probeIsolation()
		if err != nil {
			return nil, err
		}
		sb.isolation = pr
	}
	telemetry.Info(context.Background(), "sandbox: local isolation backend",
		otellog.String("session", id),
		otellog.String("backend", sb.isolation.backend.String()))
	return sb, nil
}

func (s *LocalSandbox) ID() string { return s.id }

func (s *LocalSandbox) Exec(ctx context.Context, cmd string, args []string, opts ExecOptions) (*ExecResult, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.exec",
		trace.WithAttributes(
			attribute.String("sandbox.runtime_id", s.id),
			attribute.String("sandbox.command", cmd),
			attribute.String("sandbox.isolation", s.isolation.backend.String()),
		),
	)
	defer span.End()

	if s.closed.Load() {
		span.SetStatus(codes.Error, "sandbox closed")
		return nil, ErrClosed
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	start := time.Now()
	c, err := s.buildCommand(ctx, cmd, args, opts)
	if err != nil {
		return nil, err
	}

	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = 3 * time.Second

	if s.isolation.backend != backendBubblewrap {
		c.Env = os.Environ()
		for k, v := range opts.Env {
			c.Env = append(c.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	if opts.Stdin != nil {
		c.Stdin = bytes.NewReader(opts.Stdin)
	}

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	err = c.Run()
	dur := time.Since(start)
	sbExecDuration.Record(ctx, dur.Seconds())

	result := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			span.SetAttributes(attribute.Int("sandbox.exit_code", result.ExitCode))
			sbExecCount.Add(ctx, 1, metric.WithAttributes(
				attribute.String("runtime_id", s.id),
				attribute.String("status", "nonzero_exit"),
				attribute.String("isolation", s.isolation.backend.String())))
			return result, nil
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		sbExecCount.Add(ctx, 1, metric.WithAttributes(
			attribute.String("runtime_id", s.id),
			attribute.String("status", "error"),
			attribute.String("isolation", s.isolation.backend.String())))
		return result, fmt.Errorf("sandbox: exec %s: %w", cmd, err)
	}
	span.SetAttributes(attribute.Int("sandbox.exit_code", 0))
	sbExecCount.Add(ctx, 1, metric.WithAttributes(
		attribute.String("runtime_id", s.id),
		attribute.String("status", "success"),
		attribute.String("isolation", s.isolation.backend.String())))
	return result, nil
}

func (s *LocalSandbox) buildCommand(ctx context.Context, cmd string, args []string, opts ExecOptions) (*exec.Cmd, error) {
	workDir := s.rootDir
	if opts.WorkDir != "" {
		dir, err := s.resolvePath(opts.WorkDir)
		if err != nil {
			return nil, err
		}
		workDir = dir
	}

	switch s.isolation.backend {
	case backendBubblewrap:
		env := minimalEnv(s.rootDir, opts.Env)
		c := buildBwrapCommand(ctx, s.isolation.bwrapPath,
			s.rootDir, workDir, s.readOnlyTargets, s.readWriteTargets,
			cmd, args, env, s.bwrapCfg)
		return c, nil
	default:
		c := exec.CommandContext(ctx, cmd, args...)
		c.Dir = workDir
		return c, nil
	}
}

func (s *LocalSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	resolved, err := s.resolvePath(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("sandbox: read file: %w", err)
	}
	return data, nil
}

func (s *LocalSandbox) WriteFile(_ context.Context, path string, data []byte) error {
	if s.closed.Load() {
		return ErrClosed
	}
	resolved, err := s.resolvePath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return fmt.Errorf("sandbox: create parent dirs: %w", err)
	}
	return os.WriteFile(resolved, data, 0o644)
}

func (s *LocalSandbox) Close() error {
	s.closed.Store(true)
	return nil
}

// RootDir returns the absolute path to the sandbox workspace root.
func (s *LocalSandbox) RootDir() string { return s.rootDir }

func (s *LocalSandbox) allTargets() []string {
	all := make([]string, 0, len(s.readOnlyTargets)+len(s.readWriteTargets))
	all = append(all, s.readOnlyTargets...)
	all = append(all, s.readWriteTargets...)
	return all
}

func (s *LocalSandbox) resolvePath(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		rel = strings.TrimPrefix(rel, "/")
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrPathTraversal, rel)
	}
	resolved := filepath.Join(s.rootDir, cleaned)

	target := resolved
	if _, err := os.Lstat(resolved); os.IsNotExist(err) {
		target = filepath.Dir(resolved)
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			return resolved, nil
		}
		return "", fmt.Errorf("sandbox: resolve symlinks: %w", err)
	}
	if strings.HasPrefix(real, s.rootDir) {
		return resolved, nil
	}
	for _, allowed := range s.allTargets() {
		if strings.HasPrefix(real, allowed) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("%w: symlink escape: %s", ErrPathTraversal, rel)
}
