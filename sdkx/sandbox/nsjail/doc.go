// Package nsjail implements sdk/sandbox.Runner on top of the
// nsjail (https://github.com/google/nsjail) binary — a Linux
// process isolator that wraps namespace / cgroups / seccomp / rlimits
// into a single CLI tool. It is the first isolation backend that
// actually enforces the Net and Resources policy fields of
// sandbox.ExecOptions; LocalRunner returns errdefs.NotAvailable for
// both.
//
// # Why sdkx
//
// sdk defines interfaces and primitives; sdkx ships concrete
// adapters that integrate with external systems. nsjail is an
// external binary — we shell out to it the same way sdkx/llm/openai
// shells out to the OpenAI HTTP API. The Runner type implements the
// generic sandbox.Runner interface defined in sdk/sandbox, so a
// caller can be retargeted between LocalRunner and this backend
// without changing call sites.
//
// # Linux-only
//
// nsjail uses Linux-specific features (mount / pid / net / user /
// cgroup namespaces, seccomp). The Runner type is therefore only
// constructible on Linux. On other platforms, [New] returns
// errdefs.NotAvailable so callers do not have to guard their code
// behind build tags; the resulting error is honest about why the
// backend cannot run, and macOS / Windows developers can still
// import the package for type references.
//
// # Capability matrix vs. LocalRunner
//
// Mapping of sandbox.ExecOptions fields onto nsjail flags:
//
//	WorkDir                     --cwd <dir>
//	Stdin                       piped via os/exec
//	Timeout                     --time_limit <seconds>
//	Env.Allow                   per-var --env (snapshot of host env at call time)
//	Env.Inject                  per-var --env NAME=VALUE
//	Net.Mode == NetDefault      --disable_clone_newnet (inherit host net)
//	Net.Mode == NetDenyAll      default nsjail behaviour (new net namespace, lo only)
//	Net.Mode == NetAllowList    errdefs.NotAvailable (requires iptables / nftables)
//	Net.Mode == NetProxy        errdefs.NotAvailable
//	Resources.CPUMillicores     --cgroup_cpu_ms_per_sec <value> (1000 = 1 core)
//	Resources.MemoryBytes       --cgroup_mem_max <bytes>
//	Resources.DiskBytes         errdefs.NotAvailable (would require tmpfs quota)
//	Resources.MaxOutputBytes    enforced in-process, mirroring LocalRunner
//
// # Filesystem isolation (NOT enabled in this version)
//
// This version does NOT clone the mount namespace; the child process
// sees the host filesystem. WorkDir confinement still applies via
// the same path-traversal checks LocalRunner uses on its rootDir.
// Full chroot / bind-mount support is deferred to a later version
// because it interacts with sdk/workspace's ScopedWorkspace contract
// in ways that need their own RFC. The net / cgroup / seccomp
// boundaries this backend already enforces are the principal gap
// between LocalRunner and "real" isolation, so we ship those first.
//
// # Cgroup prerequisites
//
// CPU and memory caps require cgroup v2 with delegation to the
// invoking user (typical on modern systemd hosts) OR root
// privileges. When neither is available, nsjail itself surfaces an
// error and the Runner forwards it via errdefs.Internal. The error
// message is sufficient for an operator to diagnose ("not running
// as root", "cgroup v1 detected", ...) without us re-classifying.
//
// # Binary discovery
//
// New calls exec.LookPath("nsjail") by default; pass
// [WithBinary] to point at a custom path (useful for hermetic
// builds where nsjail lives in a vendored directory). When the
// binary is not found, New returns errdefs.NotAvailable so the
// caller can decide whether to fall back to LocalRunner or refuse
// to start.
//
// # Composition
//
// The Runner is designed to be composed with the standard
// sandbox decorators:
//
//	rn := sandbox.WithDefaults(
//	    sandbox.AllowCommands(
//	        nsjail.New(rootDir),
//	        spec.AllowedCommands,
//	    ),
//	    sandbox.ExecOptions{
//	        Net:       sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
//	        Resources: sandbox.ResourceLimits{MemoryBytes: 256 << 20},
//	    },
//	)
//
// The result is a Runner whose Net / Resources policy is fixed by
// WithDefaults, whose commands are gated by AllowCommands, and
// whose Exec actually runs inside an isolated namespace.
package nsjail
