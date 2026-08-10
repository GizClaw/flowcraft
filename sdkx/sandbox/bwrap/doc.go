// Package bwrap implements sdk/sandbox.Runner on top of the
// bubblewrap (https://github.com/containers/bubblewrap) binary — a
// Linux process isolator built around user / mount / pid / net
// namespaces and bind mounts. It enforces network posture and
// filesystem bounds with Linux kernel primitives, and resource caps
// through the shared process-group watcher in sdk/sandbox.
//
// # Why sdkx
//
// sdk defines interfaces and primitives; sdkx ships concrete
// adapters that integrate with external systems. bubblewrap is an
// external binary — we shell out to it the same way sdkx/llm/openai
// shells out to the OpenAI HTTP API. The Runner type implements the
// generic sandbox.Runner interface defined in sdk/sandbox, so a
// caller can be retargeted between LocalRunner, the seatbelt backend,
// and this backend without changing call sites.
//
// # Linux-only
//
// bubblewrap uses Linux-specific features (user / mount / pid / net
// namespaces, bind mounts). The Runner type is therefore only
// constructible on Linux. On other platforms, [New] returns
// errdefs.NotAvailable so callers do not have to guard their code
// behind build tags; the resulting error is honest about why the
// backend cannot run, and macOS / Windows developers can still
// import the package for type references.
//
// # Capability matrix vs. LocalRunner
//
// Mapping of sandbox.ExecOptions fields onto the bwrap invocation:
//
//	WorkDir                     --chdir <dir>
//	Stdin                       piped via os/exec
//	Timeout                     Go-side ctx deadline + --die-with-parent (whole tree killed)
//	Env.Allow                   per-var --setenv after --clearenv (snapshot of host env at call time)
//	Env.Inject                  per-var --setenv NAME=VALUE
//	Net.Mode == NetDefault      --share-net (inherit host net)
//	Net.Mode == NetDenyAll      --unshare-net (fresh net namespace, lo only)
//	Net.Mode == NetAllowList    --unshare-net + in-netns bridge + host enforcement proxy
//	Net.Mode == NetProxy        --unshare-net + in-netns bridge + host enforcement proxy
//	Resources.CPUMillicores     shared group watcher (cpu-time = Timeout x millicores / 1000)
//	Resources.MemoryBytes       shared group watcher (aggregate group RSS)
//	Resources.DiskBytes         errdefs.NotAvailable (no quota mechanism)
//	Resources.MaxOutputBytes    enforced in-process, mirroring LocalRunner
//
// # Filesystem isolation
//
// The child gets a private mount namespace. The host root is bind-
// mounted read-only so local agents can reach the real toolchain,
// rootDir is bind-mounted read-write at the same absolute path, /tmp
// is a private writable tmpfs, and /proc plus a minimal /dev are
// freshly mounted. Additional existing paths may be opened for writing
// with [WithWritablePaths]. Mount-affecting [WithExtraFlags] values
// are rejected so callers cannot silently disable the boundary
// reported by sandbox.Enforcement.FilesystemBounds.
//
// # NetAllowList / NetProxy enforcement
//
// Both modes run the child in a fresh net namespace (only loopback, no
// default route) and force every proxy-aware client through a trusted
// gate: the bridge listens on the netns loopback and forwards each
// connection over a unix socket to a host-side enforcement proxy
// (sdkx/internal/httpkit) that evaluates the allow-list or
// forwards to the configured upstream. The bridge injects
// HTTP(S)_PROXY / ALL_PROXY and strips NO_PROXY.
//
// The bridge is not a separate executable. The runner re-executes the
// host binary itself with a reserved marker argument
// (sdkx/sandbox/bwrap/internal/bridge.Marker) — the same one-binary
// dispatch Codex uses — so the sandboxed command runs inside the host
// application. Host mains must call [MaybeBridge] as their first
// statement so the re-executed process can take over as the bridge;
// without that hook, NetAllowList / NetProxy execs fail because the
// marker is treated as an ordinary argument.
//
// The isolated net modes additionally mask /run with a private tmpfs
// (netIsolationFlags): unix sockets are not confined by network
// namespaces, so without that mask the child could reach host sockets
// (docker.sock, dbus, systemd, ...) directly. NetDefault keeps the
// host /run so host DNS (systemd's stub-resolv.conf) keeps working.
//
// Limitations (v1): only proxy-aware clients (HTTP(S)_PROXY) are
// supported — raw TCP/UDP applications fail closed; there is no UDP
// proxying; AllowHosts matches hostname suffixes or exact IP literals
// with all ports allowed. DNS resolution happens in the host proxy, so
// the child needs no resolver route.
//
// # Resource-cap caveat
//
// bwrap itself has no cgroup / rlimit controls, so CPU and memory caps
// ride on the shared process-group sampling watcher
// (sandbox.StartGroupCapsWatcher), exactly like LocalRunner and the
// seatbelt backend. That is a soft ~250ms-sampled cap rather than a
// hard cgroup limit, so Enforcement gates MemoryCap / CPUCap on
// sandbox.GroupCapsSupported instead of claiming them unconditionally.
// A future hard-cap layer could wrap bwrap with an external cgroup v2
// setup without changing the Runner seam.
//
// # Binary discovery
//
// New calls exec.LookPath("bwrap") by default; pass [WithBinary] to
// point at a custom path (useful for hermetic builds where bwrap lives
// in a vendored directory). When the binary is not found, New returns
// errdefs.NotAvailable so the caller can decide whether to fall back
// to LocalRunner or refuse to start.
//
// # Prerequisites
//
// bubblewrap is packaged in Debian, Ubuntu, and Fedora
// (package name "bubblewrap"). On kernels with unprivileged user
// namespaces enabled — the default on modern distros, GitHub Actions
// runners, and WSL2 — it runs unprivileged. On kernels without user
// namespaces the bwrap executable must be installed setuid-root, and
// a few options (e.g. --die-with-parent) become unavailable there; the
// Runner does not depend on them for the filesystem / network
// boundary, only for clean whole-tree cancellation on timeout.
//
// # Composition
//
// The Runner is designed to be composed with the standard
// sandbox decorators:
//
//	rn := sandbox.WithDefaults(
//	    sandbox.AllowCommands(
//	        bwrap.New(rootDir),
//	        spec.AllowedCommands,
//	    ),
//	    sandbox.ExecOptions{
//	        Net:       sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
//	        Resources: sandbox.ResourceLimits{MemoryBytes: 256 << 20},
//	    },
//	)
//
// The result is a Runner whose Net / Resources policy is fixed by
// WithDefaults, whose commands are gated by AllowCommands, and whose
// Exec actually runs inside an isolated namespace.
//
// # Interactive sessions
//
// Runner also implements sandbox.ProcessManager: sessions run inside
// the same bwrap invocation as Exec (same flags, same in-netns bridge
// for NetAllowList / NetProxy, with the host proxy owned by the
// session). Stdio is either a pty (TTY: true) or tagged pipes, with
// the seq-cursor replay contract defined in sdk/sandbox.
package bwrap
