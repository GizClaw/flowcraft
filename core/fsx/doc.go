// Package fsx is the file-I/O contract: one FS interface and one per-call
// Sandbox that carries an optional permission.Policy.
//
// # One value, two interpreters
//
// The same permission.Policy feeds this package and a sandbox backend. fsx
// interprets it in-process, right before the syscall; a backend projects the
// writable paths onto the kernel (bind mounts, a seatbelt profile, Windows
// ACLs). That is why the policy is an argument and not a decorator: a decorator
// holds the boundary in one process, so a remote executor or a sandboxed helper
// cannot receive it, and the second boundary has to be written from scratch.
//
// # What is enforced where
//
//   - Write entries: enforced here, and projected onto the kernel, so a
//     spawned command obeys the same set.
//   - Read and Deny entries: enforced here only. No backend projects a read
//     restriction, so a spawned command can still read a denied path. They are
//     a tool-layer guard, not the boundary that protects a secret; keep secrets
//     outside the tree instead.
//   - Containment: every path is resolved against the sandbox root and checked
//     lexically and after symlink resolution, so neither ".." nor a link
//     planted inside the tree can escape it.
//
// # Implementations
//
// [Local] is the directory-backed implementation; [Mem] is the in-memory test
// double. Both run the same contract suite (core/fsx/fstest), because an
// interface whose implementations disagree is not a contract.
//
// # What is not here
//
// Persistence (checkpoints, memory documents) and command execution
// (sandbox.Runner). This package answers one question: read and write these
// bytes, under this policy.
package fsx
