// Package lock is the crash-safe advisory lock the platform takes over a
// file that must stay single-writer: a craft DataDir+profile
// (craft/manager), a workspace state root, a GUI state root.
//
// The lock itself is a non-blocking exclusive kernel lock — flock on
// Unix, a LockFileEx byte range on Windows — so the kernel releases it
// when the holder dies for any reason, kill -9 included. A crashed
// process therefore cannot leave a lock that needs manual cleanup, which
// is the failure this package exists to end: the previous craft/manager
// lock was an O_EXCL file whose own type comment had to admit that "a
// stale lock after a crash must be removed manually".
//
// The lock file is a record, not a claim. Its content names the last
// holder (pid, kind, start time, version, optional endpoint) so a
// rejected launch can say who owns the file instead of failing on a bare
// EWOULDBLOCK; it is the last writer, not a liveness oracle — only the
// kernel lock state answers "is anyone live on this file". ReadInfo
// reads the record and says nothing about liveness.
//
// Two failures stay distinct, because callers treat them differently:
//
//   - A live holder: Acquire returns a *HeldError and IsHeld(err)
//     reports true, with the holder record attached. Refuse — something
//     else owns the file.
//   - The lock cannot be taken at all (an unlocatable filesystem, a
//     directory where the file should be, permissions, a read-only
//     mount): a plain error, IsHeld false. A caller guarding best-effort
//     work (a crash-recovery pass) fails open with a warning, because a
//     lock that cannot be taken must not keep the user out of their own
//     data; a caller guarding a single-instance contract decides for
//     itself.
//
// Release drops the kernel lock and leaves the file behind: the last
// holder's record stays readable, and the next acquisition reuses the
// inode instead of racing an unlink-and-recreate against a concurrent
// opener.
package lock
