//go:build linux

package bwrap

import (
	"maps"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// buildFlags translates sandbox.ExecOptions into the bwrap flag list
// that precedes the "--" separator. The command and its arguments are
// appended by the caller, not by this function.
//
// hostEnv mirrors os.Environ() output ("KEY=VALUE" pairs) and is
// injected by the caller so the translation stays pure for testing.
// Resource policy is not translated here: bwrap has no cgroup / rlimit
// controls, so MemoryBytes / CPUMillicores are enforced by the shared
// process-group watcher in the Runner and their validation lives in
// Exec (mirroring the seatbelt backend).
func buildFlags(opts sandbox.ExecOptions, hostEnv []string) ([]string, error) {
	flags := []string{
		// --die-with-parent: when the Go-side ctx deadline or a cap
		// overflow kills this bwrap process, the sandboxed tree dies
		// with it. This is what turns a single SIGKILL into whole-tree
		// cancellation; bwrap has no timeout flag of its own.
		"--die-with-parent",
		// --unshare-pid: fresh PID namespace (the previous backend's
		// default as well). The sandboxed command sees itself as pid 1
		// and cannot observe host processes.
		"--unshare-pid",
	}

	if opts.WorkDir != "" {
		flags = append(flags, "--chdir", opts.WorkDir)
	}

	flags = append(flags, envFlags(opts.Env, hostEnv)...)

	netF, err := netFlags(opts.Net)
	if err != nil {
		return nil, err
	}
	flags = append(flags, netF...)

	return flags, nil
}

// filesystemFlags builds the mount boundary around rootDir:
//
//   - bind the host root read-only, preserving access to toolchains;
//   - mount a private writable tmpfs at /tmp;
//   - bind rootDir and explicit exceptions read-write at the same path;
//   - mount fresh procfs and a minimal /dev.
//
// Writable binds follow the /tmp mount intentionally: when rootDir is
// itself under /tmp (common in tests and CI), the later nested bind
// makes the workspace visible inside the otherwise-private tmpfs.
func filesystemFlags(rootDir string, writable []string) []string {
	flags := []string{
		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp",
		"--bind", rootDir, rootDir,
	}
	for _, path := range writable {
		if path == "" || path == rootDir {
			continue
		}
		flags = append(flags, "--bind", path, path)
	}
	flags = append(flags, "--proc", "/proc", "--dev", "/dev")
	return flags
}

// envFlags renders sandbox.EnvPolicy as a --clearenv plus one
// --setenv NAME VALUE pair per variable. We always clear first: bwrap
// otherwise passes its own (host) environment to the child, which
// would leak variables not on the allow-list. Snapshotting the host
// env on this side keeps the policy interpretation identical to
// LocalRunner.buildEnv: Allow filters host vars, Inject layers on top
// with override semantics.
func envFlags(p sandbox.EnvPolicy, hostEnv []string) []string {
	keep := map[string]string{}

	switch {
	case p.Allow == nil:
		for _, kv := range hostEnv {
			if name, value, ok := splitKV(kv); ok {
				keep[name] = value
			}
		}
	case len(p.Allow) > 0:
		allow := make(map[string]bool, len(p.Allow))
		for _, name := range p.Allow {
			allow[name] = true
		}
		for _, kv := range hostEnv {
			if name, value, ok := splitKV(kv); ok && allow[name] {
				keep[name] = value
			}
		}
	}

	maps.Copy(keep, p.Inject)

	out := make([]string, 0, 1+2*len(keep))
	out = append(out, "--clearenv")
	for k, v := range keep {
		out = append(out, "--setenv", k, v)
	}
	return out
}

func splitKV(kv string) (string, string, bool) {
	i := strings.IndexByte(kv, '=')
	if i <= 0 {
		return "", "", false
	}
	return kv[:i], kv[i+1:], true
}

func netFlags(p sandbox.NetPolicy) ([]string, error) {
	switch p.Mode {
	case sandbox.NetDefault:
		// Inherit the host's network namespace.
		return []string{"--share-net"}, nil
	case sandbox.NetDenyAll, sandbox.NetAllowList, sandbox.NetProxy:
		// Fresh net namespace with only loopback (bwrap brings lo up).
		// NetAllowList / NetProxy differ only in the command (the
		// in-netns bridge) and the extra binds, which Exec adds.
		return []string{"--unshare-net"}, nil
	default:
		return nil, errdefs.NotAvailablef("bwrap: unknown net mode %d", int(p.Mode))
	}
}

// netIsolationFlags returns the extra namespace hardening needed for
// the isolated net modes: a private tmpfs at /run hides host unix
// sockets (docker.sock, dbus, systemd, ...), which --unshare-net does
// not confine. NetDefault keeps the host /run so host DNS (e.g.
// systemd's stub-resolv.conf under /run) keeps working; the isolated
// modes have no resolver route anyway and resolve through the
// enforcement proxy on the host.
func netIsolationFlags(mode sandbox.NetMode) []string {
	switch mode {
	case sandbox.NetDenyAll, sandbox.NetAllowList, sandbox.NetProxy:
		return []string{"--tmpfs", "/run"}
	default:
		return nil
	}
}

// rejectedExtraFlags is every bwrap option that can downgrade a policy
// dimension the Runner promises to enforce (filesystem bounds, env
// allow-list, net posture, namespace isolation, workdir confinement,
// seccomp, capability drops) or smuggle extra options past validation
// (--args). The list is intentionally exhaustive: WithExtraFlags is an
// escape hatch for logging / diagnostics, not for policy.
var rejectedExtraFlags = map[string]bool{
	// filesystem / mount
	"--ro-bind": true, "--ro-bind-try": true,
	"--bind": true, "--bind-try": true,
	"--dev-bind": true, "--dev-bind-try": true,
	"--bind-fd": true, "--ro-bind-fd": true,
	"--remount-ro":  true,
	"--overlay-src": true, "--overlay": true, "--tmp-overlay": true, "--ro-overlay": true,
	"--proc": true, "--dev": true, "--tmpfs": true, "--mqueue": true,
	"--dir": true, "--file": true, "--bind-data": true, "--ro-bind-data": true,
	"--symlink": true, "--chmod": true, "--perms": true,
	// env
	"--clearenv": true, "--setenv": true, "--unsetenv": true,
	// net / namespaces
	"--unshare-all": true, "--share-net": true,
	"--unshare-user": true, "--unshare-user-try": true,
	"--unshare-ipc": true, "--unshare-pid": true, "--unshare-net": true,
	"--unshare-uts": true, "--unshare-cgroup": true, "--unshare-cgroup-try": true,
	"--disable-userns": true, "--assert-userns-disabled": true,
	"--userns": true, "--userns2": true, "--pidns": true,
	// workdir / identity / seccomp / caps / session
	"--chdir": true, "--uid": true, "--gid": true, "--hostname": true,
	"--seccomp": true, "--add-seccomp-fd": true,
	"--cap-add": true, "--cap-drop": true,
	"--new-session": true,
	// arg smuggling
	"--args": true,
}

// validateExtraFlags rejects any extra flag that could weaken the
// boundary. Callers get a Validation error at construction time instead
// of a silently downgraded policy at Exec time.
func validateExtraFlags(flags []string) error {
	for _, raw := range flags {
		name := raw
		if index := strings.IndexByte(name, '='); index >= 0 {
			name = name[:index]
		}
		if rejectedExtraFlags[name] {
			return errdefs.Validationf(
				"bwrap: extra flag %q can weaken sandbox enforcement; policy flags are owned by ExecOptions",
				raw,
			)
		}
	}
	return nil
}
