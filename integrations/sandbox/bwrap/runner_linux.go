//go:build linux

package bwrap

import (
	"context"
	"crypto/x509"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/integrations/sandbox/bwrap/internal/bridge"
	"github.com/GizClaw/flowcraft/integrations/sandbox/internal/httpkit"
	"github.com/GizClaw/flowcraft/integrations/sandbox/internal/httpkit/mitm"
)

const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// sandboxProxySock is where the host enforcement proxy's unix socket
// is bind-mounted inside the sandbox for NetAllowList / NetProxy execs.
const sandboxProxySock = "/run/flowcraft-proxy.sock"

// Runner is a bubblewrap-backed core/sandbox.Runner. It is only
// constructible on Linux; see [New] for non-Linux behaviour.
type Runner struct {
	rootDir          string
	binary           string
	extraFlags       []string
	writablePaths    []string
	defaultMaxOutput int64
	sessions         *sandbox.SessionRegistry
	decision         func(httpkit.ProxyDecision)
	hooks            mitm.ProxyHooks
	outboundRoots    *x509.CertPool
}

// Enforcement reports the dimensions bwrap plus the shared
// process-group watcher enforce in this backend.
func (r *Runner) Enforcement() sandbox.Enforcement {
	caps := sandbox.GroupCapsSupported()
	return sandbox.Enforcement{
		EnvAllowList:     true,
		NetModes:         []sandbox.NetMode{sandbox.NetDenyAll, sandbox.NetAllowList, sandbox.NetProxy},
		Socks5:           true,
		MITM:             true,
		UnixSocketPolicy: true,
		MemoryCap:        caps,
		CPUCap:           caps,
		FilesystemBounds: true,
	}
}

// New returns a Runner that confines child processes with bubblewrap.
func New(rootDir string, opts ...RunnerOption) (*Runner, error) {
	cfg := &runnerConfig{}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
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
		outboundRoots:    cfg.roots,
	}
	runner.sessions = sandbox.NewSessionRegistry(runner.spawnProcess)
	return runner, nil
}

// Capabilities implements core/sandbox.Runner.
func (r *Runner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		Policy: r.Enforcement(),
		Features: sandbox.SessionFeatures{
			TTY:    true,
			Signal: true,
			Events: true,
		},
	}
}

// Exec runs cmd once through the session path.
func (r *Runner) Exec(
	ctx context.Context,
	cmd string,
	args []string,
	opts sandbox.ExecOptions,
) (*sandbox.ExecResult, error) {
	return sandbox.Exec(ctx, r, cmd, args, opts)
}

// Start implements core/sandbox.Runner.
func (r *Runner) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	return r.sessions.Start(ctx, spec)
}

// List implements core/sandbox.Runner.
func (r *Runner) List(ctx context.Context) ([]sandbox.SessionInfo, error) {
	return r.sessions.List(ctx)
}

// Terminate implements core/sandbox.Runner.
func (r *Runner) Terminate(ctx context.Context, id string) error {
	return r.sessions.Terminate(ctx, id)
}

// spawnProcess is the core sandbox SessionStarter. It builds the bwrap
// invocation from the SessionSpec and hands it to the shared session
// implementation.
func (r *Runner) spawnProcess(
	ctx context.Context,
	spec sandbox.SessionSpec,
) (sandbox.Session, error) {
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

	proxyMode := spec.Opts.Net.Mode == sandbox.NetAllowList ||
		spec.Opts.Net.Mode == sandbox.NetProxy
	var proxy *httpkit.Proxy
	if proxyMode {
		proxy, err = httpkit.Start(httpkit.ProxyConfig{
			Mode:          spec.Opts.Net.Mode,
			AllowHosts:    spec.Opts.Net.AllowHosts,
			Rules:         spec.Opts.Net.Rules,
			Upstream:      spec.Opts.Net.Proxy,
			MITM:          spec.Opts.Net.MITM,
			OnDecision:    r.decision,
			Hooks:         r.hooks,
			OutboundRoots: r.outboundRoots,
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
		maxOut = 0
	}
	spec.Opts.Resources.MaxOutputBytes = maxOut

	sess, err := sandbox.StartSession(ctx, spec, c)
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
			_, _ = sess.Wait(context.Background())
			cleanup.once.Do(cleanup.cleanup)
		}()
		return &sessionHandle{Session: sess, cleanup: cleanup}, nil
	}
	return sess, nil
}

// sessionHandle keeps a core Session's side resources (the host
// enforcement proxy) alive for exactly as long as the session.
type sessionHandle struct {
	sandbox.Session
	cleanup *sessionCleanup
}

func (s *sessionHandle) Close() error {
	err := s.Session.Close()
	if s.cleanup != nil {
		s.cleanup.once.Do(s.cleanup.cleanup)
	}
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
// uses.
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

var _ sandbox.Runner = (*Runner)(nil)
