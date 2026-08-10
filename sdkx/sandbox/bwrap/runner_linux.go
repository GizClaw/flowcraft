//go:build linux

package bwrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdkx/internal/httpkit"
	"github.com/GizClaw/flowcraft/sdkx/internal/httpkit/mitm"
	"github.com/GizClaw/flowcraft/sdkx/sandbox/bwrap/internal/bridge"
)

const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// sandboxProxySock is where the host enforcement proxy's unix socket
// is bind-mounted inside the sandbox for NetAllowList / NetProxy execs.
// It lives under /run, which the isolated net modes mask with a fresh
// tmpfs (see netIsolationFlags).
const sandboxProxySock = "/run/flowcraft-proxy.sock"

// Runner is a bubblewrap-backed sandbox.Runner. It is only
// constructible on Linux; see [New] for non-Linux behaviour.
type Runner struct {
	rootDir          string
	binary           string
	extraFlags       []string
	writablePaths    []string
	defaultMaxOutput int64
	processes        sandbox.ProcessManager
	decision         func(httpkit.ProxyDecision)
	hooks            mitm.ProxyHooks
}

// Enforcement reports the dimensions bwrap plus the shared
// process-group watcher enforce in this backend. Resource caps come
// from the watcher, so they are claimed only when that watcher is
// actually operable here — see sandbox.GroupCapsSupported.
func (r *Runner) Enforcement() sandbox.Enforcement {
	caps := sandbox.GroupCapsSupported()
	return sandbox.Enforcement{
		EnvAllowList: true,
		NetModes: []sandbox.NetMode{
			sandbox.NetDenyAll,
			sandbox.NetAllowList,
			sandbox.NetProxy,
		},
		Socks5:           true,
		MITM:             true,
		UnixSocketPolicy: true,
		MemoryCap:        caps,
		CPUCap:           caps,
		FilesystemBounds: true,
	}
}

// New returns a Runner that confines child processes with bubblewrap.
// rootDir bounds WorkDir resolution exactly as it does for
// sandbox.LocalRunner.
//
// Errors:
//   - errdefs.NotAvailable when the bwrap binary cannot be found
//     (caller can fall back to LocalRunner or refuse to start).
//   - errdefs.Validation when rootDir cannot be resolved or a
//     policy-weakening [WithExtraFlags] value is supplied.
//
// The returned Runner is safe for concurrent use.
func New(rootDir string, opts ...RunnerOption) (*Runner, error) {
	cfg := &runnerConfig{}
	for _, o := range opts {
		o(cfg)
	}
	if err := validateExtraFlags(cfg.extra); err != nil {
		return nil, err
	}

	binary := cfg.binFrom
	if binary == "" {
		resolved, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, errdefs.NotAvailablef(
				"bwrap: binary not found on PATH; install bubblewrap or use WithBinary")
		}
		binary = resolved
	} else if _, err := exec.LookPath(binary); err != nil {
		// Validate up front so a misconfigured path surfaces at
		// construction time, not at the first Exec.
		return nil, errdefs.NotAvailablef(
			"bwrap: binary %q not executable: %v", binary, err)
	}

	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, errdefs.Validationf("bwrap: resolve rootDir: %v", err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = resolved
	}

	writable := make([]string, 0, len(cfg.writable))
	seenWritable := map[string]bool{abs: true}
	for _, path := range cfg.writable {
		resolved, err := resolveConfiguredPath(path)
		if err != nil {
			return nil, err
		}
		if !seenWritable[resolved] {
			seenWritable[resolved] = true
			writable = append(writable, resolved)
		}
	}

	runner := &Runner{
		rootDir:          abs,
		binary:           binary,
		extraFlags:       append([]string(nil), cfg.extra...),
		writablePaths:    writable,
		defaultMaxOutput: defaultMaxOutputBytes,
		decision:         cfg.decision,
		hooks:            cfg.hooks,
	}
	runner.processes = sandbox.NewProcessRegistry(runner.spawnProcess)
	return runner, nil
}

// Exec runs cmd with args inside a bubblewrap invocation that enforces
// opts.Net at the kernel level and opts.Resources through the shared
// process-group watcher. The function never downgrades policy: opts
// the backend cannot honour cause errdefs.NotAvailable, never a silent
// best-effort run.
func (r *Runner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	if cmd == "" {
		return nil, errdefs.Validationf("bwrap: empty command")
	}
	if err := sandbox.ValidateExecPolicy(opts); err != nil {
		return nil, err
	}

	resolvedWorkDir, err := r.resolveWorkDir(opts.WorkDir)
	if err != nil {
		return nil, err
	}
	// Push the resolved WorkDir back so bwrap receives an absolute,
	// vetted path rather than the caller's raw input.
	opts.WorkDir = resolvedWorkDir

	proxyMode := opts.Net.Mode == sandbox.NetAllowList || opts.Net.Mode == sandbox.NetProxy
	var proxy *httpkit.Proxy
	if proxyMode {
		var err error
		proxy, err = httpkit.Start(httpkit.ProxyConfig{
			Mode:       opts.Net.Mode,
			AllowHosts: opts.Net.AllowHosts,
			Rules:      opts.Net.Rules,
			Upstream:   opts.Net.Proxy,
			MITM:       opts.Net.MITM,
			OnDecision: r.decision,
			Hooks:      r.hooks,
		})
		if err != nil {
			return nil, errdefs.Internalf("bwrap: start enforcement proxy: %v", err)
		}
		defer func() { _ = proxy.Close() }()
	}

	var bundlePath string
	var bundleCleanup func()
	if opts.Net.MITM != nil && opts.Net.MITM.Enabled {
		if proxy == nil {
			return nil, errdefs.NotAvailablef(
				"bwrap: MITM requires allow_list or proxy net mode")
		}
		bundlePath, bundleCleanup, err = mitm.WriteBundle(proxy.CAPEM())
		if err != nil {
			return nil, errdefs.Internalf("bwrap: write CA bundle: %v", err)
		}
		defer bundleCleanup()
		if opts.Env.Inject == nil {
			opts.Env.Inject = make(map[string]string)
		} else {
			opts.Env.Inject = maps.Clone(opts.Env.Inject)
		}
		opts.Env.Inject["SSL_CERT_FILE"] = bundlePath
	}

	flags, err := buildFlags(opts, os.Environ())
	if err != nil {
		return nil, err
	}
	fsFlags := filesystemFlags(r.rootDir, r.writablePaths)
	fsFlags = append(fsFlags, netIsolationFlags(opts.Net.Mode)...)
	if bundlePath != "" {
		fsFlags = append(fsFlags, "--ro-bind", bundlePath, bundlePath)
	}
	for _, path := range opts.Net.UnixSockets {
		if _, statErr := os.Stat(path); statErr != nil {
			return nil, errdefs.NotFoundf(
				"bwrap: allowed unix socket %q does not exist: %v", path, statErr)
		}
		fsFlags = append(fsFlags, "--bind", path, path)
	}

	// The post-"--" argv. For NetAllowList / NetProxy the command is
	// the in-netns bridge, which then runs the real command as its
	// child. The bridge is not a separate executable: the host binary
	// is re-executed with a reserved marker argument (mirroring how
	// Codex dispatches on argv[0]) and runs the in-netns bridge logic
	// from sdkx/sandbox/bwrap/internal/bridge. The running executable
	// and the host proxy socket are bind-mounted in so they are
	// reachable regardless of where they live on the host (e.g. under
	// /tmp, which the sandbox masks).
	var command []string
	if proxyMode {
		exe, err := os.Executable()
		if err != nil {
			return nil, errdefs.Internalf(
				"bwrap: resolve host binary for in-netns bridge: %v", err)
		}
		fsFlags = append(fsFlags,
			"--ro-bind", exe, exe,
			"--bind", proxy.SocketPath(), sandboxProxySock,
		)
		command = append([]string{
			exe, bridge.Marker, "--sock", sandboxProxySock, "--", cmd,
		}, args...)
	} else {
		command = append([]string{cmd}, args...)
	}

	// Extra flags come first so the auto-generated policy flags always
	// win on a conflict (bwrap applies options in order).
	argv := append([]string{}, r.extraFlags...)
	argv = append(argv, flags...)
	argv = append(argv, fsFlags...)
	argv = append(argv, "--")
	argv = append(argv, command...)

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	c := exec.CommandContext(ctx, r.binary, argv...)
	// bwrap itself runs in the host env; the child's env is shaped by
	// --clearenv / --setenv inside buildFlags.
	c.Env = os.Environ()
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// --die-with-parent already kills the tree when this process dies;
	// killing the whole group as well closes the tiny setup window and
	// matches how the cap watcher terminates the group.
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = 2 * time.Second

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

	if err := c.Start(); err != nil {
		return nil, classifyStartError(cmd, err)
	}
	watcher := sandbox.StartGroupCapsWatcher(ctx, c.Process.Pid, opts.Resources, opts.Timeout)

	runErr := c.Wait()
	watcher.Stop()
	result := &sandbox.ExecResult{
		Stdout: stdout.buf.String(),
		Stderr: stderr.buf.String(),
	}
	if runErr != nil {
		// Checked before Exceeded: a watcher that gave up on sampling
		// killed the group without proving any budget was exceeded.
		if sampleErr := watcher.Unenforceable(); sampleErr != nil {
			return result, errdefs.NotAvailablef(
				"bwrap: resource caps became unenforceable while executing %s: %v", cmd, sampleErr)
		}
		if cap := watcher.Exceeded(); cap != "" {
			return result, errdefs.BudgetExceededf(
				"bwrap: %s resource cap exceeded while executing %s", cap, cmd)
		}
		if ctx.Err() != nil {
			return result, errdefs.FromContext(fmt.Errorf("bwrap: exec %s: %w", cmd, ctx.Err()))
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, classifyStartError(cmd, runErr)
	}
	return result, nil
}

// Start implements sandbox.ProcessManager. The session runs inside the
// same bubblewrap invocation as Exec; the in-netns bridge and host
// enforcement proxy, when NetAllowList / NetProxy needs them, are
// owned by the session and closed when the session exits or is closed.
func (r *Runner) Start(ctx context.Context, spec sandbox.ProcessSpec) (sandbox.Process, error) {
	return r.processes.Start(ctx, spec)
}

// List implements sandbox.ProcessManager.
func (r *Runner) List(ctx context.Context) ([]sandbox.ProcessInfo, error) {
	return r.processes.List(ctx)
}

// Terminate implements sandbox.ProcessManager.
func (r *Runner) Terminate(ctx context.Context, id string) error {
	return r.processes.Terminate(ctx, id)
}

// spawnProcess is the Runner's sandbox.ProcessStarter. It reuses the
// exact flag assembly Exec uses (buildFlags + filesystemFlags +
// netIsolationFlags + bridge re-exec), then hands the bwrap command to
// the shared session implementation for pty/pipe stdio.
func (r *Runner) spawnProcess(ctx context.Context, spec sandbox.ProcessSpec) (sandbox.Process, error) {
	if len(spec.Argv) == 0 {
		return nil, errdefs.Validationf("bwrap: empty command")
	}
	if err := sandbox.ValidateExecPolicy(spec.Opts); err != nil {
		return nil, err
	}

	resolvedWorkDir, err := r.resolveWorkDir(spec.Opts.WorkDir)
	if err != nil {
		return nil, err
	}
	spec.Opts.WorkDir = resolvedWorkDir

	proxyMode := spec.Opts.Net.Mode == sandbox.NetAllowList || spec.Opts.Net.Mode == sandbox.NetProxy
	var proxy *httpkit.Proxy
	if proxyMode {
		proxy, err = httpkit.Start(httpkit.ProxyConfig{
			Mode:       spec.Opts.Net.Mode,
			AllowHosts: spec.Opts.Net.AllowHosts,
			Rules:      spec.Opts.Net.Rules,
			Upstream:   spec.Opts.Net.Proxy,
			MITM:       spec.Opts.Net.MITM,
			OnDecision: r.decision,
			Hooks:      r.hooks,
		})
		if err != nil {
			return nil, errdefs.Internalf("bwrap: start enforcement proxy: %v", err)
		}
	}

	var bundlePath string
	var bundleCleanup func()
	if spec.Opts.Net.MITM != nil && spec.Opts.Net.MITM.Enabled {
		if proxy == nil {
			return nil, errdefs.NotAvailablef(
				"bwrap: MITM requires allow_list or proxy net mode")
		}
		bundlePath, bundleCleanup, err = mitm.WriteBundle(proxy.CAPEM())
		if err != nil {
			_ = proxy.Close()
			return nil, errdefs.Internalf("bwrap: write CA bundle: %v", err)
		}
		if spec.Opts.Env.Inject == nil {
			spec.Opts.Env.Inject = make(map[string]string)
		} else {
			spec.Opts.Env.Inject = maps.Clone(spec.Opts.Env.Inject)
		}
		spec.Opts.Env.Inject["SSL_CERT_FILE"] = bundlePath
	}
	abortBundle := func() {
		if bundleCleanup != nil {
			bundleCleanup()
		}
	}

	flags, err := buildFlags(spec.Opts, os.Environ())
	if err != nil {
		if proxy != nil {
			_ = proxy.Close()
		}
		abortBundle()
		return nil, err
	}
	fsFlags := filesystemFlags(r.rootDir, r.writablePaths)
	fsFlags = append(fsFlags, netIsolationFlags(spec.Opts.Net.Mode)...)
	if bundlePath != "" {
		fsFlags = append(fsFlags, "--ro-bind", bundlePath, bundlePath)
	}
	for _, path := range spec.Opts.Net.UnixSockets {
		if _, statErr := os.Stat(path); statErr != nil {
			if proxy != nil {
				_ = proxy.Close()
			}
			abortBundle()
			return nil, errdefs.NotFoundf(
				"bwrap: allowed unix socket %q does not exist: %v", path, statErr)
		}
		fsFlags = append(fsFlags, "--bind", path, path)
	}

	var command []string
	if proxyMode {
		exe, err := os.Executable()
		if err != nil {
			if proxy != nil {
				_ = proxy.Close()
			}
			abortBundle()
			return nil, errdefs.Internalf(
				"bwrap: resolve host binary for in-netns bridge: %v", err)
		}
		fsFlags = append(fsFlags,
			"--ro-bind", exe, exe,
			"--bind", proxy.SocketPath(), sandboxProxySock,
		)
		command = append([]string{
			exe, bridge.Marker, "--sock", sandboxProxySock, "--", spec.Argv[0],
		}, spec.Argv[1:]...)
	} else {
		command = append([]string{spec.Argv[0]}, spec.Argv[1:]...)
	}

	argv := append([]string{}, r.extraFlags...)
	argv = append(argv, flags...)
	argv = append(argv, fsFlags...)
	argv = append(argv, "--")
	argv = append(argv, command...)

	c := exec.Command(r.binary, argv...)
	c.Env = os.Environ()

	maxOut := spec.Opts.Resources.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = r.defaultMaxOutput
	}
	if maxOut <= 0 {
		maxOut = math.MaxInt64
	}
	spec.Opts.Resources.MaxOutputBytes = maxOut

	proc, err := sandbox.StartSession(ctx, c, spec.Opts, spec.TTY, spec.Rows, spec.Cols)
	if err != nil {
		if proxy != nil {
			_ = proxy.Close()
		}
		abortBundle()
		return nil, err
	}
	if proxy != nil {
		cleanup := &sessionCleanup{cleanup: func() {
			_ = proxy.Close()
			abortBundle()
		}}
		go func() {
			_, _ = proc.Wait(context.Background())
			cleanup.once.Do(cleanup.cleanup)
		}()
		return &sessionProcess{Process: proc, cleanup: cleanup}, nil
	}
	return proc, nil
}

// sessionProcess keeps a sandbox.Process's side resources (the host
// enforcement proxy) alive for exactly as long as the session.
type sessionProcess struct {
	sandbox.Process
	cleanup *sessionCleanup
}

func (p *sessionProcess) Close() error {
	err := p.Process.Close()
	p.cleanup.once.Do(p.cleanup.cleanup)
	return err
}

type sessionCleanup struct {
	once    sync.Once
	cleanup func()
}

func resolveConfiguredPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errdefs.Validationf("bwrap: resolve writable path %q: %v", path, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errdefs.Validationf(
			"bwrap: writable path %q must exist: %v", path, err)
	}
	return real, nil
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
		return "", fmt.Errorf("bwrap: resolve workdir: %w", err)
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

// classifyStartError maps process-start failures onto errdefs
// categories, mirroring sandbox.LocalRunner.
func classifyStartError(cmd string, err error) error {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errdefs.NotFound(fmt.Errorf("bwrap: exec %s: %w", cmd, err))
	case errors.Is(err, os.ErrPermission):
		return errdefs.Forbidden(fmt.Errorf("bwrap: exec %s: %w", cmd, err))
	default:
		return errdefs.Internal(fmt.Errorf("bwrap: exec %s: %w", cmd, err))
	}
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
