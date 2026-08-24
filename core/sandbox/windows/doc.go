// Package windows implements core/sandbox.Runner on top of Windows
// sandboxing primitives: restricted tokens, DACL-based filesystem
// boundaries, and Job Objects. It is the Windows sibling of
// core/sandbox/bwrap and core/sandbox/seatbelt: same policy surface,
// different enforcement kernel.
//
// # Why not a namespace backend
//
// Windows has no user-namespace / mount-namespace primitive
// comparable to bubblewrap, and no seatbelt equivalent. The closest
// OS-level boundaries are:
//
//   - a restricted token (CreateRestrictedToken: privileges disabled,
//     WRITE_RESTRICTED/LUA) so the child cannot elevate or escape to
//     administrative surfaces;
//   - DACL-based filesystem rules: workspace and writable roots grant
//     access to random per-workspace capability SIDs, and protected
//     paths get deny-write / deny-read ACEs. This is a defense-in-depth
//     ACL boundary between same-user processes — deliberately weaker
//     than a namespace, and documented as such;
//   - a Job Object: process-tree termination (KILL_ON_JOB_CLOSE) plus
//     hard Memory/CPU limits (SetInformationJobObject + completion-port
//     watcher);
//   - ConPTY for interactive TTY sessions.
//
// The design follows codex-rs (codex-rs/windows-sandbox-rs): an
// unelevated restricted-token backend for the default case, with an
// elevated setup helper + WFP filters for network policy landing in
// P2.
//
// # Windows-only
//
// The Runner type is only constructible on windows; on other
// platforms [New] returns errdefs.NotAvailable so callers can import
// the package for type references without build-tag gymnastics.
//
// # Capability matrix vs. core/sandbox/local
//
// Mapping of sandbox.ExecOptions fields onto the Windows mechanisms:
//
//	WorkDir                     -- chdir; writes confined to the ACL-protected root
//	Stdin                       piped via os/exec
//	Timeout                     Go-side ctx deadline + job-object kill
//	Env.Allow / Env.Inject      filtered in Go (cmd.Env)
//	Net.Mode == NetDefault      host posture
//	Net.Mode != NetDefault      errdefs.NotAvailable until the elevated/WFP backend (P2)
//	Resources.MemoryBytes       job-object hard limit (JOB_OBJECT_LIMIT_JOB_MEMORY)
//	Resources.CPUMillicores     job-object sampled cpu-time watcher
//	Resources.DiskBytes         errdefs.NotAvailable (no quota mechanism)
//	Resources.MaxOutputBytes    truncated in Go (sandbox.Exec)
//
// # Status
//
// P1 is implemented: restricted token + workspace capability SIDs,
// DACL-based workspace ACLs (allow-write on root/writable roots,
// deny-write on .codex/.agents), job-object process-tree termination
// with hard MemoryBytes / sampled CPUMillicores caps, and ConPTY/pipe
// sessions with the shared seq-cursor replay contract.
//
// P2 (elevated backend + WFP network policy) is implemented:
//
//   - two dedicated local accounts (FlowCraftSbxOffline /
//     FlowCraftSbxOnline) created by an elevated setup step,
//     DPAPI-protected credentials, and a setup marker;
//   - persistent WFP filters on the offline account: catch-all block
//     for outbound connects and inbound accepts (IPv4 + IPv6) with an
//     explicit loopback allow, so only the host-side enforcement
//     proxy is reachable;
//   - a re-executed helper (windows.MaybeHelper at the top of main,
//     mirroring bwrap's bridge hook) serving spawn requests over a
//     named pipe (framed JSON, seq-cursor replay); the runner
//     re-executes itself elevated once per Runner via a single UAC
//     prompt — no separate binary to deploy;
//   - network policy: NetDenyAll (WFP alone), NetAllowList / NetProxy
//     (WFP + the core/utils/net enforcement proxy, with Socks5 and
//     MITM inherited from the proxy). NetDefault runs children as the
//     online account.
//
// Known limitations: deny-read carveouts (P2c) and private desktop /
// hide-users polish are not implemented yet; Capabilities claims
// NetModes only after the WFP setup marker is present.
//
// # Security notes
//
// The helper pipe is protected by a user-scoped DACL plus a
// per-runner secret carried through an environment variable that is
// removed immediately after launch; the server additionally bounds
// every path it touches (Root / Cwd / WritableRoots) to the launch
// root, so a same-user process cannot turn the elevated helper into
// an arbitrary ACL-mutation primitive. Writable roots configured
// outside the workspace root are rejected on the elevated path.
//
// The sandbox accounts and WFP filter keys are machine-wide, so the
// elevated backend is single-user per machine: a second user's setup
// resets the shared accounts and invalidates the first user's
// DPAPI-protected credentials. Multi-user deployments need per-user
// account namespacing (future work).
//
// If the user cancels the UAC prompt, the Runner caches the launch
// error for its lifetime; if the helper exits (idle timeout, crash,
// or manual close), the next spawn relaunches it once (another UAC
// prompt) and fails the call if that also fails.
//
// Known P1 limitations: .bat/.cmd command files are not auto-wrapped
// in cmd.exe /c (the manual CreateProcessAsUserW path does not do the
// wrapping os/exec does), and capability SIDs are regenerated per
// Runner, so a restart appends fresh ACL entries instead of reusing
// persisted ones (dead ACEs from older SIDs are harmless but
// accumulate).
//
// # Interactive sessions
//
// Runner sessions (sandbox.Runner.Start) run under the same
// restricted token as Exec (including the job object), with ConPTY
// (TTY) or tagged pipes and the seq-cursor replay contract defined in
// core/sandbox.
//
// # Verification
//
// What is exercised where:
//
//   - compile + vet: every platform via make ci / GOOS=windows build;
//   - unit tests: pure logic (IPC framing, outputBuffer semantics,
//     proxy env injection, helper arg parsing) plus Runner
//     construction, which really applies workspace ACLs and resolves
//     the workspace SIDs — run on the windows-sandbox CI lane;
//   - integration (go test -tags=integration_windows on a Windows
//     host; CI lane test-sandbox-windows-integration): real
//     restricted-token spawns, env/workdir policy, timeout, Job
//     Object CPU/memory caps, a ConPTY TTY session, and — only when
//     the process token is elevated — account creation, WFP filter
//     installation, elevated helper spawns as the offline/online
//     accounts, and an outbound-block probe. The elevated tests
//     mutate the machine (two local accounts + persistent WFP
//     filters) and skip themselves when not elevated.
//
// Manual checks for the parts integration tests cannot reach (UAC
// interaction, interactive ConPTY behavior, proxy policy):
//
//   - after elevated setup: net user FlowCraftSbxOffline /
//     FlowCraftSbxOnline must exist, and
//     %APPDATA%\flowcraft\windows-sandbox must contain creds.json and
//     setup.json;
//   - WFP filters: netsh wfp show filters file=wfp.xml, then grep the
//     dump for "FlowCraft";
//   - run an interactive TUI in a TTY session and confirm resize and
//     Ctrl-C behave like a real terminal;
//   - with NetProxy / NetAllowList, run curl through the sandbox and
//     confirm egress is limited to allow-listed destinations.
package windows
