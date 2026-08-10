// Package sandbox is the agent's execution boundary: where commands run,
// what they can reach (net), what they can see (env), and how much they
// can consume (resources). Sandbox is daemon-level shared policy; per-run
// state lives in sdk/workspace.
//
// The package centres on the Runner interface, a single Exec call that
// turns a command + arguments + ExecOptions into an ExecResult. Concrete
// runners differ in *where* the work happens (local process,
// bubblewrap namespace, container, microVM) but share the same policy
// surface so a
// caller can be retargeted between backends without changing call sites.
//
// ExecOptions carries three policy groups beyond the obvious WorkDir /
// Stdin / Timeout knobs:
//
//   - Env (EnvPolicy): explicit allow-list of host environment variables
//     plus an Inject map. Replaces "inherit the entire daemon's env" which
//     is unsafe in a multi-tenant agent harness.
//   - Net (NetPolicy): mode + (future) allow-list / proxy URL. LocalRunner
//     only accepts NetDefault; non-default modes require a sandboxing
//     backend (namespace-based, container-based, or microVM-based) that
//     can actually enforce the policy at the kernel level.
//   - Resources (ResourceLimits): CPU / memory / disk caps plus
//     MaxOutputBytes. On unix, LocalRunner enforces group-wide memory
//     and cpu-time caps with a sampling watcher and kills the whole
//     process group on overflow. DiskBytes still needs a quota-capable
//     backend and is rejected with errdefs.NotAvailable.
//
// EnforcementOf lets callers inspect the honest policy surface before
// execution. LocalRunner reports env + process-group resource
// enforcement but not filesystem or network confinement. Concrete
// sdkx backends add those OS-level boundaries:
//
//	                         LocalRunner  seatbelt/macOS  bubblewrap/Linux
//	Env allow-list               yes           yes             yes
//	Filesystem write bounds       no           yes             yes
//	NetDenyAll                     no           yes             yes
//	MemoryBytes                   yes           yes             yes
//	CPUMillicores                 yes           yes             yes
//	DiskBytes                      no            no              no
//
// WithDefaults fixes daemon-owned policy, AllowCommands adds a hard
// command-name gate, and WithApproval adds a fail-closed human decision
// tripwire. The recommended local composition lives in
// sdkx/sandbox.ComposeLocal.
//
// # Long-running sessions
//
// Runners may additionally implement ProcessManager (discovered with
// ProcessManagerOf) to spawn interactive or streaming processes under
// the same ExecOptions policy. Policy is fixed once at Start — env,
// network posture, resource caps, and approval are never re-negotiated
// per Read/Write. Output is a byte-cursor log: Read(afterSeq) replays
// from any retained position, bounded by Resources.MaxOutputBytes, so
// a reconnecting client resumes without re-running the process.
// Backends without the capability (or without a pty) return
// errdefs.NotAvailable rather than silently downgrading to Exec. The
// decorators implement ProcessManager as well, so interactive sessions
// cannot bypass the defaults / approval / allow-list chain.
package sandbox
