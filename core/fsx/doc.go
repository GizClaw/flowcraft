// Package fsx is the file-I/O contract: one FS interface, one Policy
// value, and one per-call Sandbox context that carries the policy.
//
// # Why the policy is an argument
//
// The boundary is not a property of the file system object; it is a
// property of the call. Binding a policy to an instance (a decorator)
// works only while everything runs in one process, and breaks the moment
// the operations run somewhere else: a remote executor cannot receive a
// wrapper, but it can receive a serialisable context. Every FS method
// therefore takes a *Sandbox, and an implementation is free to enforce
// it in-process (checking before touching the disk) or to hand the
// context to a supervising process that enforces it at the kernel.
//
// # What is not here
//
// No storage backends (checkpoints, memory documents) and no boundary
// for process execution. Persistence is a separate port with its own
// lifetime; command execution is sandbox.Runner. This package answers one
// question: read and write these bytes, under this policy.
package fsx
