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
// # Phase-1 capability matrix
//
//	WorkDir / Stdin / Timeout     fully supported. Every child runs
//	                              inside a job object; timeout, cancel
//	                              and close terminate the whole job
//	                              (all processes, not just the leader).
//	Env allow-list / inject       fully supported (EnvPolicy).
//	Net.Mode != NetDefault        errdefs.NotAvailable (no AppContainer
//	                              / Windows Filtering Platform backend
//	                              yet).
//	Write == WriteReadOnly        errdefs.NotAvailable (no OS-level
//	                              write confinement yet; phase 2 adds
//	                              restricted tokens + ACLs).
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
// # Files
//
//   - Runner: runner.go / runner_other.go
//   - Options: options.go
//   - Job objects: job_windows.go
//   - Sessions: session_windows.go
//   - Deployment resource: register.go
package windows
