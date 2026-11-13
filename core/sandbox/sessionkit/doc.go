// Package sessionkit is the shared process-supervision machinery every
// core/sandbox backend builds on: it turns a [sandbox.SessionSpec] into a
// running [sandbox.Session], and it owns the bookkeeping that must be identical
// across backends.
//
// It is public because a backend lives outside this directory tree
// (core/sandbox-<impl>) and Go's internal rule would otherwise put it out of
// reach. A backend that spawns processes should reuse this instead of writing
// its own supervisor: the contract's session semantics are subtle, and two
// implementations drift.
//
// # What it provides
//
//   - [NewSessionRegistry] is the session table: caller-supplied or generated
//     ids with duplicate detection, List ordering, Terminate-by-id, and the
//     Close drain. A backend supplies one [SessionStarter] — its "spawn this
//     argv under this policy" function — and inherits the rest.
//   - [StartSession] is the unix spawn: pipe versus pty, the /dev/null stdin
//     rule, output collection into the replay log, and the per-session
//     capabilities. A backend that prepares an *exec.Cmd (its own flags,
//     environment and confinement) hands it here.
//   - [StartGroupCapsWatcher] samples the child's process group so
//     ResourceLimits.MemoryBytes and CPUMillicores can be enforced, and
//     [GroupCapsAvailable] reports whether that sampler works on this host. A
//     backend must gate its Enforcement.MemoryCap / Enforcement.CPUCap on that
//     answer instead of hardcoding true, otherwise it advertises a cap that
//     silently never fires.
//
// # What it does not decide
//
// Policy. A backend validates its own requests (engine.ValidateExecPolicy plus
// its platform posture), decides how to confine the child, and only then calls
// StartSession. Nothing here widens a boundary: the /dev/null stdin rule and the
// output ring are the same for every backend, but the confinement is the
// backend's own.
//
// # Output is a log, not a stream
//
// A session's output is an append-only log addressed by a monotonic sequence
// number, bounded by the backend's ring size. Read is a cursor operation over
// that log, the process's exit does not close it, and a reader that falls behind
// the retained window gets [sandbox.ErrSequenceGap] rather than a silent hole.
// Watch replays from a cursor and then follows live; a subscriber that
// overflows is told where to resume (sandbox.SessionEventLag) instead of losing
// bytes.
package sessionkit
