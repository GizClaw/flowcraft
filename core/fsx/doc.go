// Package fsx is where file I/O happens: reads go through this process, writes
// go through a sandbox.
//
// # Why the split
//
// A read restriction only means something if the kernel enforces it, and this
// framework does not project one. So reads are unrestricted: any absolute path
// this process can open, fsx can read. That is a decision with a consequence —
// the model can read a secret and copy it into the workspace — and it is stated
// here rather than left to be discovered.
//
// Writes are the opposite. A write is the one file operation where a mistake
// becomes persistent, so it is performed by a helper process running inside the
// sandbox, with the writable set projected onto the kernel. The helper does no
// policy check of its own: the kernel is the boundary, and this package never
// has to prove that a path is safe to write. A path spelling this process does
// not understand cannot widen anything, because nothing here decides.
//
// # What that costs
//
// Every write spawns a process (roughly 5–50ms depending on the backend). A
// tool that writes many small files should batch its work, not call Write in a
// loop. Reads, listing, walking and stat are in-process and cheap.
//
// # Without a sandbox
//
// A host with no sandbox (a local workspace demo, a test) cannot have kernel
// enforcement. It must say so: Open fails unless the host opts into
// [Unenforced], in which case writes go straight to the filesystem and the
// writable set is documentation, not a boundary.
//
// # The helper
//
// Writes are performed by re-executing the host binary with a magic argument
// and speaking JSON over stdin/stdout. A host must call [MaybeRunHelper] before
// doing anything else in main; forgetting it makes the first write fail with a
// clear error rather than silently writing unconfined.
package fsx
