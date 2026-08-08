//go:build darwin

package seatbelt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdkx/internal/httpkit"
)

const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// Runner confines child processes with macOS Seatbelt.
type Runner struct {
	rootDir          string
	binary           string
	writable         []string
	defaultMaxOutput int64
}

// New constructs a Seatbelt Runner rooted at rootDir.
func New(rootDir string, opts ...RunnerOption) (*Runner, error) {
	cfg := &runnerConfig{}
	for _, option := range opts {
		option(cfg)
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

	return &Runner{
		rootDir:          root,
		binary:           binary,
		writable:         dedupe(writable),
		defaultMaxOutput: defaultMaxOutputBytes,
	}, nil
}

// Enforcement reports the policy dimensions Seatbelt plus the shared
// process-group watcher enforce on macOS. The Seatbelt profile itself
// covers net and filesystem, but resource caps come from the shared
// watcher, so they are claimed only when that watcher is actually
// operable here — see sandbox.GroupCapsSupported.
func (r *Runner) Enforcement() sandbox.Enforcement {
	caps := sandbox.GroupCapsSupported()
	return sandbox.Enforcement{
		EnvAllowList: true,
		NetModes: []sandbox.NetMode{
			sandbox.NetDenyAll,
			sandbox.NetAllowList,
			sandbox.NetProxy,
		},
		MemoryCap:        caps,
		CPUCap:           caps,
		FilesystemBounds: true,
	}
}

// Exec runs cmd inside a generated Seatbelt profile.
func (r *Runner) Exec(ctx context.Context, cmd string, args []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	if cmd == "" {
		return nil, errdefs.Validationf("seatbelt: empty command")
	}
	if opts.Resources.DiskBytes != 0 {
		return nil, errdefs.NotAvailablef(
			"seatbelt: disk limits not supported (no quota mechanism)",
		)
	}
	if opts.Resources.CPUMillicores != 0 && opts.Timeout <= 0 {
		return nil, errdefs.NotAvailablef(
			"seatbelt: CPUMillicores requires a per-call Timeout to derive a cpu-time cap",
		)
	}
	// Memory and cpu caps ride on the shared sampling watcher, not on
	// the Seatbelt profile. Where that watcher cannot sample, honouring
	// the call would run the child with no cap at all, so reject it
	// instead of pretending.
	if (opts.Resources.MemoryBytes > 0 || opts.Resources.CPUMillicores > 0) && !sandbox.GroupCapsSupported() {
		return nil, errdefs.NotAvailablef(
			"seatbelt: resource limits require process-group sampling, which is unavailable here",
		)
	}

	workDir, err := r.resolveWorkDir(opts.WorkDir)
	if err != nil {
		return nil, err
	}

	// NetAllowList / NetProxy run through a host-side enforcement proxy
	// on loopback; the profile opens exactly that port. Proxy mode
	// forwards to the configured upstream from the host, so the SBPL
	// rule is the same loopback hole for both modes.
	proxyMode := opts.Net.Mode == sandbox.NetAllowList || opts.Net.Mode == sandbox.NetProxy
	var proxy *httpkit.Proxy
	proxyPort := 0
	if proxyMode {
		proxy, err = httpkit.Start(httpkit.ProxyConfig{
			Mode:        opts.Net.Mode,
			AllowHosts:  opts.Net.AllowHosts,
			Upstream:    opts.Net.Proxy,
			TCPLoopback: true,
		})
		if err != nil {
			return nil, errdefs.Internalf("seatbelt: start enforcement proxy: %v", err)
		}
		defer func() { _ = proxy.Close() }()
		addr, ok := proxy.Addr().(*net.TCPAddr)
		if !ok {
			return nil, errdefs.Internalf("seatbelt: proxy bound %T, want TCP loopback", proxy.Addr())
		}
		proxyPort = addr.Port
	}

	profile, err := buildProfile(r.writable, opts.Net, proxyPort)
	if err != nil {
		return nil, err
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	argv := []string{"-p", profile, cmd}
	argv = append(argv, args...)
	c := exec.CommandContext(ctx, r.binary, argv...)
	c.Dir = workDir
	c.Env = buildEnv(opts.Env, proxyPort)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
				"seatbelt: resource caps became unenforceable while executing %s: %v", cmd, sampleErr)
		}
		if cap := watcher.Exceeded(); cap != "" {
			return result, errdefs.BudgetExceededf(
				"seatbelt: %s resource cap exceeded while executing %s", cap, cmd)
		}
		if ctx.Err() != nil {
			return result, errdefs.FromContext(
				fmt.Errorf("seatbelt: exec %s: %w", cmd, ctx.Err()),
			)
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, classifyStartError(cmd, runErr)
	}
	return result, nil
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
// When proxyPort is non-zero (NetAllowList / NetProxy), it additionally
// forces every proxy-aware client onto the enforcement proxy loopback
// port and strips NO_PROXY so clients cannot opt out into a connection
// the SBPL profile would deny anyway. These injections override the
// caller's env (same rule as Env.Inject: sandbox-level policy wins).
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

func classifyStartError(cmd string, err error) error {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errdefs.NotFound(fmt.Errorf("seatbelt: exec %s: %w", cmd, err))
	case errors.Is(err, os.ErrPermission):
		return errdefs.Forbidden(fmt.Errorf("seatbelt: exec %s: %w", cmd, err))
	default:
		return errdefs.Internal(fmt.Errorf("seatbelt: exec %s: %w", cmd, err))
	}
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

type limitedBuffer struct {
	buf bytes.Buffer
	max int64
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if b.max <= 0 || int64(b.buf.Len()) >= b.max {
		return len(data), nil
	}
	space := b.max - int64(b.buf.Len())
	if int64(len(data)) <= space {
		return b.buf.Write(data)
	}
	if _, err := b.buf.Write(data[:space]); err != nil {
		return 0, err
	}
	return len(data), nil
}
