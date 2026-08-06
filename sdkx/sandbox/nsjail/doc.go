// Package nsjail implements sdk/sandbox.Runner on top of the
// nsjail (https://github.com/google/nsjail) binary — a Linux
// process isolator that wraps namespace / cgroups / seccomp / rlimits
// into a single CLI tool. It enforces network posture and resource
// policy with Linux kernel primitives. LocalRunner now offers portable
// process-group resource caps via a watcher, but still cannot enforce
// network isolation; nsjail adds that boundary with namespaces and
// cgroups.
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
// # Filesystem isolation
//
// The child gets a private mount namespace. The host root stays visible
// read-only so local agents can reach the real toolchain, rootDir is
// bind-mounted read-write at the same absolute path, and /tmp is a
// private writable tmpfs. Additional existing paths may be opened for
// writing with [WithWritablePaths]. Mount-affecting [WithExtraFlags]
// values are rejected so callers cannot silently disable the boundary
// reported by sandbox.Enforcement.FilesystemBounds.
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
