//go:build darwin

package seatbelt

import (
	"context"
	"crypto/x509"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
	"github.com/GizClaw/flowcraft/core/utils/net/mitm"
)

const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// Runner confines child processes with macOS Seatbelt and implements
// core/sandbox.Runner.
type Runner struct {
	rootDir          string
	binary           string
	writable         []string
	defaultMaxOutput int64
	sessions         *sandbox.SessionRegistry
	decision         func(corenet.ProxyDecision)
	hooks            corenet.MITMHooks
	outboundRoots    *x509.CertPool
}

// New constructs a Seatbelt Runner rooted at rootDir.
func New(rootDir string, opts ...RunnerOption) (*Runner, error) {
	cfg := &runnerConfig{}
	for _, option := range opts {
		if option != nil {
			option(cfg)
		}
	}

	binary := cfg.binFrom
	if binary == "" {
		resolved, err := exec.LookPath("sandbox-exec")
		if err != nil {
			return nil, errdefs.NotAvailablef(
				"seatbelt: sandbox-exec not found; this macOS installation cannot enforce Seatbelt profiles",
			)
		}
		binary = resolved
	} else if _, err := exec.LookPath(binary); err != nil {
		return nil, errdefs.NotAvailablef(
			"seatbelt: binary %q not executable: %v", binary, err,
		)
	}

	root, err := resolveRoot(rootDir)
	if err != nil {
		return nil, err
	}
	writable := []string{root}
	for _, path := range cfg.writable {
		resolved, err := resolveRoot(path)
		if err != nil {
			return nil, fmt.Errorf("seatbelt: resolve writable path %q: %w", path, err)
		}
		writable = append(writable, resolved)
	}

	runner := &Runner{
		rootDir:          root,
		binary:           binary,
		writable:         dedupe(writable),
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
	caps := sandbox.GroupCapsSupported()
	return sandbox.Capabilities{
		Policy: sandbox.Enforcement{
			EnvAllowList:     true,
			NetModes:         []corenet.NetMode{corenet.NetDenyAll, corenet.NetAllowList, corenet.NetProxy},
			Socks5:           true,
			MITM:             true,
			MemoryCap:        caps,
			CPUCap:           caps,
			FilesystemBounds: true,
		},
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

// Close implements core/sandbox.Runner: it terminates every session
// started through this runner. Safe to call more than once.
func (r *Runner) Close() error {
	return r.sessions.Close()
}

// spawnProcess is the core sandbox SessionStarter.
func (r *Runner) spawnProcess(
	ctx context.Context,
	spec sandbox.SessionSpec,
) (sandbox.Session, error) {
	if len(spec.Argv) == 0 {
		return nil, errdefs.Validationf("seatbelt: empty command")
	}
	if len(spec.Opts.Net.UnixSockets) > 0 {
		return nil, errdefs.NotAvailablef(
			"seatbelt: unix socket allow-list is not supported (SBPL does not confine unix sockets)")
	}
	if err := sandbox.ValidateExecPolicy(spec.Opts); err != nil {
		return nil, err
	}

	workDir, err := r.resolveWorkDir(spec.Opts.WorkDir)
	if err != nil {
		return nil, err
	}

	proxyMode := spec.Opts.Net.Mode == corenet.NetAllowList ||
		spec.Opts.Net.Mode == corenet.NetProxy
	var proxy *corenet.Proxy
	proxyPort := 0
	if proxyMode {
		proxy, err = corenet.Start(corenet.ProxyConfig{
			Mode:          spec.Opts.Net.Mode,
			AllowHosts:    spec.Opts.Net.AllowHosts,
			Rules:         spec.Opts.Net.Rules,
			Upstream:      spec.Opts.Net.Proxy,
			TCPLoopback:   true,
			MITM:          spec.Opts.Net.MITM,
			OnDecision:    r.decision,
			Hooks:         r.hooks,
			MITMFactory:   mitm.New,
			OutboundRoots: r.outboundRoots,
		})
		if err != nil {
			return nil, errdefs.Internalf("seatbelt: start enforcement proxy: %v", err)
		}
		addr, ok := proxy.Addr().(*net.TCPAddr)
		if !ok {
			closeProxy(ctx, proxy)
			return nil, errdefs.Internalf(
				"seatbelt: proxy bound %T, want TCP loopback", proxy.Addr())
		}
		proxyPort = addr.Port
	}

	var bundlePath string
	var bundleCleanup func()
	if spec.Opts.Net.MITM != nil && spec.Opts.Net.MITM.Enabled {
		if proxy == nil {
			return nil, errdefs.NotAvailablef(
				"seatbelt: MITM requires allow_list or proxy net mode")
		}
		bundlePath, bundleCleanup, err = mitm.WriteBundle(proxy.CAPEM())
		if err != nil {
			closeProxy(ctx, proxy)
			return nil, errdefs.Internalf("seatbelt: write CA bundle: %v", err)
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

	profile, err := buildProfile(r.writable, spec.Opts.Net, proxyPort)
	if err != nil {
		closeProxy(ctx, proxy)
		abortBundle()
		return nil, err
	}

	argv := []string{"-p", profile, spec.Argv[0]}
	argv = append(argv, spec.Argv[1:]...)
	c := exec.Command(r.binary, argv...)
	c.Dir = workDir
	c.Env = buildEnv(spec.Opts.Env, proxyPort)
	if bundlePath != "" {
		c.Env = append(c.Env, "SSL_CERT_FILE="+bundlePath)
	}

	maxOut := spec.Opts.Resources.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = r.defaultMaxOutput
	}
	spec.Opts.Resources.MaxOutputBytes = maxOut

	sess, err := sandbox.StartSession(ctx, spec, c)
	if err != nil {
		closeProxy(ctx, proxy)
		abortBundle()
		return nil, err
	}
	if proxy != nil {
		cleanup := &sessionCleanup{cleanup: func() {
			closeProxy(context.Background(), proxy)
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

// closeProxy best-effort closes the enforcement proxy, leaving the
// error visible to telemetry. It is nil-safe for uniform use on every
// spawnProcess teardown path.
func closeProxy(ctx context.Context, proxy *corenet.Proxy) {
	if proxy == nil {
		return
	}
	if err := proxy.Close(); err != nil {
		telemetry.WarnErr(ctx, "seatbelt: close enforcement proxy failed", err)
	}
}

// sessionHandle keeps a core Session's side resources alive.
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
		return "", fmt.Errorf("seatbelt: resolve workdir: %w", err)
	}
	if real != r.rootDir && !strings.HasPrefix(real, r.rootDir+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: workdir %q escapes root", sandbox.ErrPathTraversal, dir)
	}
	return abs, nil
}

func resolveRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errdefs.Validationf("seatbelt: resolve path %q: %v", path, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errdefs.Validationf("seatbelt: path %q must exist: %v", path, err)
	}
	return real, nil
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

// buildEnv constructs the child environment from the caller's EnvPolicy.
func buildEnv(policy sandbox.EnvPolicy, proxyPort int) []string {
	values := map[string]string{}
	switch {
	case policy.Allow == nil:
		for _, kv := range os.Environ() {
			if key, value, ok := splitKV(kv); ok {
				values[key] = value
			}
		}
	case len(policy.Allow) > 0:
		allowed := make(map[string]bool, len(policy.Allow))
		for _, name := range policy.Allow {
			allowed[name] = true
		}
		for _, kv := range os.Environ() {
			if key, value, ok := splitKV(kv); ok && allowed[key] {
				values[key] = value
			}
		}
	}
	for key, value := range policy.Inject {
		values[key] = value
	}
	if proxyPort > 0 {
		proxy := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
		for _, name := range []string{
			"HTTP_PROXY", "http_proxy",
			"HTTPS_PROXY", "https_proxy",
			"ALL_PROXY", "all_proxy",
		} {
			values[name] = proxy
		}
		delete(values, "NO_PROXY")
		delete(values, "no_proxy")
		values["NO_PROXY"] = ""
		values["no_proxy"] = ""
	}
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func splitKV(kv string) (string, string, bool) {
	index := strings.IndexByte(kv, '=')
	if index <= 0 {
		return "", "", false
	}
	return kv[:index], kv[index+1:], true
}

func dedupe(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}

var _ sandbox.Runner = (*Runner)(nil)
