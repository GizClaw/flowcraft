//go:build windows

package windows

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
	"github.com/GizClaw/flowcraft/core/utils/net/mitm"
)

const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// Runner confines child processes with a restricted token, workspace
// ACLs, and a Job Object, and implements core/sandbox.Runner.
type Runner struct {
	rootDir          string
	writable         []string
	caps             *capabilitySIDs
	level            string
	defaultMaxOutput int64
	sessions         *sandbox.SessionRegistry
	registryOnce     sync.Once

	elevatedOnce   sync.Once
	elevatedMu     sync.Mutex
	elevatedPipe   string
	elevatedSecret string
	elevatedErr    error
	elevatedRel    chan struct{} // closed when an in-flight relaunch settles
}

// New constructs a Windows Runner rooted at rootDir. The root is
// resolved via filepath.Abs + EvalSymlinks so a later symlink swap on
// the root itself cannot be used to escape.
func New(rootDir string, opts ...RunnerOption) (*Runner, error) {
	cfg := &runnerConfig{}
	for _, option := range opts {
		if option != nil {
			option(cfg)
		}
	}

	root, err := resolveRoot(rootDir)
	if err != nil {
		return nil, err
	}
	writable := []string{root}
	for _, path := range cfg.writable {
		resolved, err := resolveRoot(path)
		if err != nil {
			return nil, err
		}
		writable = append(writable, resolved)
	}
	caps, err := newCapabilitySIDs()
	if err != nil {
		return nil, err
	}
	if err := applyWorkspaceACLs(root, writable, caps); err != nil {
		return nil, fmt.Errorf("windows: apply workspace acls: %w", err)
	}

	runner := &Runner{
		rootDir:          root,
		writable:         writable,
		caps:             caps,
		level:            cfg.level,
		defaultMaxOutput: defaultMaxOutputBytes,
	}
	if runner.level == "" {
		runner.level = LevelRestrictedToken
	}
	if cfg.setDefaultMaxOutput {
		if cfg.defaultMaxOutput <= 0 {
			runner.defaultMaxOutput = math.MaxInt64
		} else {
			runner.defaultMaxOutput = cfg.defaultMaxOutput
		}
	}
	runner.sessions = sandbox.NewSessionRegistry(runner.spawnProcess)
	return runner, nil
}

// Capabilities declares Runner's surface: env allow-list filtering,
// job-object resource caps, DACL-based filesystem bounds (applied at
// construction), and ConPTY/pipe sessions with events and Ctrl-C.
func (r *Runner) Capabilities() sandbox.Capabilities {
	policy := sandbox.Enforcement{
		EnvAllowList:     true,
		MemoryCap:        jobObjectCapsAvailable(),
		CPUCap:           jobObjectCapsAvailable(),
		FilesystemBounds: true,
	}
	if r.level == LevelElevated {
		dir, err := sandboxConfigDir()
		if err == nil && setupComplete(dir) {
			policy.NetModes = []corenet.NetMode{
				corenet.NetDenyAll,
				corenet.NetAllowList,
				corenet.NetProxy,
			}
			policy.Socks5 = true
			policy.MITM = true
		}
	}
	return sandbox.Capabilities{
		Policy: policy,
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
	return r.registry().Start(ctx, spec)
}

// List implements core/sandbox.Runner.
func (r *Runner) List(ctx context.Context) ([]sandbox.SessionInfo, error) {
	return r.registry().List(ctx)
}

// Terminate implements core/sandbox.Runner.
func (r *Runner) Terminate(ctx context.Context, id string) error {
	return r.registry().Terminate(ctx, id)
}

// Close implements core/sandbox.Runner: it terminates every session
// started through this runner. Safe to call more than once and when
// the runner never started anything.
func (r *Runner) Close() error {
	err := r.registry().Close()
	if r.level == LevelElevated {
		r.sendHelperShutdown()
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
// Windows policy surface, builds the configured command, attaches the
// restricted token, and hands the result to the shared Windows
// session implementation (which owns stdio, the job object, and
// reaping).
func (r *Runner) spawnProcess(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if err := validateExecPolicy(spec.Opts); err != nil {
		return nil, err
	}
	workDir, err := resolveWorkDir(r.rootDir, spec.Opts.WorkDir)
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
	if r.level == LevelElevated {
		return r.spawnElevated(ctx, spec, workDir)
	}
	if spec.Opts.Net.Mode != corenet.NetDefault {
		return nil, errdefs.NotAvailablef(
			"windows: net policy requires the elevated backend (level %q)", LevelElevated)
	}
	cmd.Env = buildEnv(spec.Opts.Env)
	if err := applyProtectedDenies(workDir, r.caps); err != nil {
		return nil, err
	}
	tok, err := restrictedTokenForSpec(r.caps)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tok.Close() }()
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(tok)}
	return sandbox.StartWindowsSession(ctx, spec, cmd)
}

// spawnElevated is the level-elevated spawn path: it validates the
// requested net posture, starts the host-side enforcement proxy for
// NetAllowList / NetProxy (WFP confines the offline account to
// loopback), and routes the session through the elevated runner.
func (r *Runner) spawnElevated(ctx context.Context, spec sandbox.SessionSpec, workDir string) (sandbox.Session, error) {
	mode := spec.Opts.Net.Mode
	switch mode {
	case corenet.NetDefault, corenet.NetDenyAll, corenet.NetAllowList, corenet.NetProxy:
	default:
		return nil, errdefs.NotAvailablef("windows: unsupported net mode %v", mode)
	}
	if spec.Opts.Net.MITM != nil && spec.Opts.Net.MITM.Enabled &&
		mode != corenet.NetAllowList && mode != corenet.NetProxy {
		return nil, errdefs.NotAvailablef(
			"windows: MITM requires allow_list or proxy net mode")
	}

	env := buildEnv(spec.Opts.Env)
	account := sandboxAccountOnline
	var proxy *corenet.Proxy
	proxyPort := 0
	var bundleCleanup func()
	if mode == corenet.NetAllowList || mode == corenet.NetProxy {
		account = sandboxAccountOffline
		var err error
		proxy, err = corenet.Start(corenet.ProxyConfig{
			Mode:        mode,
			AllowHosts:  spec.Opts.Net.AllowHosts,
			Rules:       spec.Opts.Net.Rules,
			Upstream:    spec.Opts.Net.Proxy,
			TCPLoopback: true,
			MITM:        spec.Opts.Net.MITM,
			MITMFactory: mitm.New,
		})
		if err != nil {
			return nil, errdefs.Internalf("windows: start enforcement proxy: %v", err)
		}
		addr, ok := proxy.Addr().(*net.TCPAddr)
		if !ok {
			_ = proxy.Close()
			return nil, errdefs.Internalf("windows: proxy bound %T, want TCP loopback", proxy.Addr())
		}
		proxyPort = addr.Port
		env = injectProxyEnv(env, proxyPort)
		if spec.Opts.Net.MITM != nil && spec.Opts.Net.MITM.Enabled {
			bundle, cleanup, err := mitm.WriteBundle(proxy.CAPEM())
			if err != nil {
				_ = proxy.Close()
				return nil, errdefs.Internalf("windows: write CA bundle: %v", err)
			}
			bundleCleanup = cleanup
			env = append(env, "SSL_CERT_FILE="+bundle)
		}
	}

	cleanup := func() {
		if proxy != nil {
			_ = proxy.Close()
		}
		if bundleCleanup != nil {
			bundleCleanup()
		}
	}
	sess, err := r.startElevated(ctx, spec, workDir, env, account)
	if err != nil {
		cleanup()
		return nil, err
	}
	wrapped := &cleanupSession{Session: sess, cleanup: cleanup}
	// Release the proxy (and MITM bundle) as soon as the session ends,
	// even when the caller never invokes Close.
	go func() {
		_, _ = sess.Wait(context.Background())
		wrapped.once.Do(cleanup)
	}()
	return wrapped, nil
}

// helperExePath resolves the current executable for the re-exec
// helper launch. The elevated backend re-executes the host binary
// with HelperArgvMarker (mirroring the bwrap bridge), so no separate
// helper binary needs to be deployed.
func (r *Runner) helperExePath() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "flowcraft-sandbox-helper"
}

var _ sandbox.Runner = (*Runner)(nil)
