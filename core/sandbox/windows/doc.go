// Package windows implements core/sandbox.Runner on top of the
// Windows Job Object API: process-tree lifecycle
// (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE), job-wide memory caps, and
// whole-tree termination on timeout / cancel / close. It is the
// Windows sibling of core/sandbox/local, core/sandbox/bwrap (Linux)
// and core/sandbox/seatbelt (macOS): same policy surface, different
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
//	Net.Mode != NetDefault        errdefs.NotAvailable (no AppContainer
//	                              / Windows Filtering Platform backend
//	                              yet).
//	Write == WriteReadOnly        enforced when the runner is built
//	                              with WithWriteConfinement: the child
//	                              runs with a restricted, Low-integrity
//	                              token and only the explicitly granted
//	                              paths are writable. Without the
//	                              option the call fails with
//	                              errdefs.NotAvailable.
//	Resources.MemoryBytes         enforced as a job-wide memory limit
//	                              (JOB_OBJECT_LIMIT_JOB_MEMORY).
//	Resources.CPUMillicores       enforced as a per-process user-time
//	                              limit (JOB_OBJECT_LIMIT_PROCESS_TIME)
//	                              derived from Timeout x millicores /
//	                              1000; requires Timeout > 0, otherwise
//	                              errdefs.NotAvailable. The limit is
//	                              per process, not aggregate across the
//	                              job.
//	Resources.DiskBytes != 0      errdefs.NotAvailable (no quota
//	                              mechanism).
//	Resources.MaxOutputBytes      enforced in-process, mirroring
//	                              core/sandbox/local.
//	Sessions                      pipe-only (no pty). TTY, Resize,
//	                              Signal and Watch return
//	                              errdefs.NotAvailable; Terminate maps
//	                              to TerminateJobObject (Windows has no
//	                              SIGTERM equivalent).
//	Exit classification           a process killed by a job memory or
//	                              cpu-time cap is reported as a plain
//	                              SessionExited with the job exit code,
//	                              not SessionBudgetExceeded (no
//	                              limit-violation notification port is
//	                              wired up yet).
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
//   - Deployment resource: register.go
package windows
