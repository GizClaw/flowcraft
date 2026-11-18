// Package windows implements core/sandbox.Runner on top of the
// Windows Job Object API: process-tree lifecycle
// (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE), job-wide memory caps, and
// whole-tree termination on timeout / cancel / close. It is the
// Windows sibling of core/sandbox-local, core/sandbox-bwrap (Linux)
// and core/sandbox-seatbelt (macOS): same policy surface, different
// enforcement kernel.
//
// # Windows-only
//
// The Runner is only constructible on Windows. On other platforms New
// returns errdefs.NotAvailable so callers can import the package for
// type references without build-tag gymnastics (the same pattern as
// the bwrap and seatbelt backends).
//
// # Capability matrix
//
//	WorkDir / Stdin / Timeout     fully supported. Every child runs
//	                              inside a job object; timeout, cancel
//	                              and close terminate the whole job
//	                              (all processes, not just the leader).
//	Env allow-list / inject       fully supported (EnvPolicy).
//	Net.Mode                     NetDefault (host networking),
//	                              NetDenyAll, NetAllowList and
//	                              NetProxy are enforced. Every
//	                              non-default mode runs the child
//	                              under an AppContainer token scoped to
//	                              one per-runner profile. NetDenyAll
//	                              runs without any network capability,
//	                              so the OS firewall's AppIsolation
//	                              default-deny blocks TCP and connected
//	                              flows at the kernel. Because that
//	                              default does not constrain unconnected
//	                              UDP or ICMP sockets, every non-default
//	                              mode also installs WFP bind-layer
//	                              (ALE_RESOURCE_ASSIGNMENT) filters for
//	                              the package SID: NetDenyAll blocks
//	                              every bind, while NetAllowList /
//	                              NetProxy permit only TCP binds, pin
//	                              the container to a host-side
//	                              enforcement proxy (only the loopback
//	                              proxy port is reachable), and inject
//	                              the proxy environment
//	                              (HTTP(S)_PROXY -> http://loopback,
//	                              ALL_PROXY -> socks5://loopback; the
//	                              listener multiplexes both protocols,
//	                              so SOCKS5-aware non-HTTP clients
//	                              traverse the same allow-list /
//	                              upstream). The proxy-port permit is
//	                              installed at both ALE_AUTH_CONNECT
//	                              and ALE_AUTH_RECV_ACCEPT: the latter
//	                              layer is where the built-in
//	                              AppContainerLoopback block lives, so
//	                              a connect-layer permit alone would
//	                              make the proxy connection time out.
//	                              Every non-default mode
//	                              also runs a behavioral fence probe
//	                              under the container token before the
//	                              isolation is handed out: a dial to a
//	                              non-proxy loopback port must be
//	                              blocked, an unconnected UDP send must
//	                              be blocked (covering the bind-layer
//	                              filters), and in allow-list / proxy
//	                              modes a dial to the proxy port must
//	                              succeed. A mismatch fails closed.
//	                              Hosts without AppContainer-profile or
//	                              WFP-engine access fail closed with
//	                              errdefs.NotAvailable.
//	Permission != nil             enforced when the runner is built
//	                              with WithWriteConfinement: the child
//	                              runs with a restricted, Low-integrity
//	                              token and only the explicitly granted
//	                              paths are writable. Without the
//	                              option the call fails with
//	                              errdefs.NotAvailable.
//	Resources.MemoryBytes         enforced by sampling the job's peak
//	                              committed memory every 25ms, and
//	                              terminating the tree when it passes
//	                              the budget — the same shape as the
//	                              unix watcher, so a session may run
//	                              past its budget until the next
//	                              sample. Deliberately *not* a
//	                              kernel limit: JOB_OBJECT_LIMIT_JOB_
//	                              MEMORY refuses the allocation
//	                              instead of ending the process, so
//	                              the job never crosses the budget
//	                              and nothing can report the breach.
//	Resources.CPUMillicores       enforced as a job-wide user-time
//	                              budget (JOB_OBJECT_LIMIT_JOB_TIME)
//	                              derived from Timeout x millicores /
//	                              1000, matching the unix watcher's
//	                              aggregate-group semantics; requires
//	                              Timeout > 0, otherwise
//	                              errdefs.NotAvailable.
//	Resources.DiskBytes != 0      errdefs.NotAvailable (no quota
//	                              mechanism).
//	Resources.MaxOutputBytes      enforced in-process, mirroring
//	                              core/sandbox-local.
//	Sessions                      pipe sessions (separate stdout /
//	                              stderr streams) and TTY sessions
//	                              through ConPTY (merged
//	                              SessionStreamTTY output plus Resize).
//	                              Signal and Watch return
//	                              errdefs.NotAvailable; Terminate maps
//	                              to TerminateJobObject (Windows has no
//	                              SIGTERM equivalent). TTY combined
//	                              with WithWriteConfinement is
//	                              NotAvailable until the
//	                              restricted-token ConPTY spawn path is
//	                              verified against a real console.
//	Exit classification           exceeding a budget ends the session
//	                              as SessionBudgetExceeded: a cpu
//	                              budget through the kernel's
//	                              JOB_OBJECT_MSG_END_OF_JOB_TIME,
//	                              a memory budget through the
//	                              sampler. A cap that stops being
//	                              measurable ends the session as
//	                              SessionUnenforceable with an
//	                              errdefs NotAvailable error once
//	                              maxCapSampleFailures samples in a
//	                              row fail, or when the cpu
//	                              verdict channel dies before the
//	                              session ends — running a child
//	                              unguarded under a capability that
//	                              still claims the cap is the
//	                              failure the contract exists to
//	                              prevent. A session that reaches
//	                              its Timeout with a memory budget
//	                              configured records the job's peak
//	                              as telemetry, because an exceeded
//	                              budget that was never sampled and a
//	                              slow child look the same at that
//	                              point.
//
// # File journal
//
// A runner can be built with [WithFileJournal] to report the writes made
// under its root and its explicitly writable paths (see
// core/sandbox/journal): the source is the host's
// ReadDirectoryChangesW, so the host-side watch sees a sandboxed
// process's writes without any cooperation from it, and sees the host
// process's writes into the same tree too. It is an observation stream,
// never a boundary — write confinement still decides what is allowed,
// and the journal reports what happened. On this platform a watch costs
// one directory watch plus the change buffer the kernel pins for its
// pending read, which is what
// [sandbox.JournalCapabilities.WatchBudget] counts here.
//
// # Write confinement notes
//
// WithWriteConfinement, the runner root (plus WithWritablePaths
// entries) is labeled Low integrity (SYSTEM_MANDATORY_LABEL_ACE with
// NO_WRITE_UP) so the Low-integrity child can write there and read
// everywhere else. Caveats:
//
//   - The label changes are persistent on the workspace directories
//     (they are not reverted on Close); this is the same "the
//     workspace is writable by low-integrity processes" model as a
//     Low-integrity cache dir.
//   - The child gets its own Low-labeled TEMP/TMP scratch dir
//     (flowcraft-low-*) because the user's Medium temp is
//     write-denied for a Low subject; it is removed on Runner.Close.
//   - CreateProcessAsUser requires the host to hold
//     SE_INCREASE_QUOTA_NAME; ordinary desktop processes usually do
//     not have it, so write-confined spawns may fail with
//     errdefs.NotAvailable unless the host runs elevated or as a
//     service. The restricted token is derived directly from the
//     caller's primary token so the SE_ASSIGNPRIMARYTOKEN_NAME
//     exemption applies. The backend fails closed rather than
//     degrading to an unsandboxed spawn.
//   - CreateProcessAsUser places the child on a non-interactive window
//     station, so GUI applications will not be visible. Console /
//     CLI tools are unaffected.
//
// # Files
//
//   - Runner: runner.go / runner_other.go
//   - Options: options.go
//   - Job objects: job_windows.go
//   - Sessions: session_windows.go
//   - ConPTY: conpty_windows.go
//   - File journal: journal_windows_test.go (contract suite),
//     journal_settings_test.go (deployment settings)
//   - Deployment resource: register.go
package windows
