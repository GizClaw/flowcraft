// Package delegation defines backend-neutral contracts for assigning work to
// another agent or execution target.
//
// Mode distinguishes synchronous calls, interaction handoffs, and asynchronous
// work. Directory owns discovery; Service owns execution and status lookup.
// Concrete local, queued, and remote implementations belong outside this
// package.
//
// The package also provides the unified "delegate" handoff tool plus static
// and directory-backed agent referees. A host can expose a Service to
// execution-time tools with [WithService], and consumers recover it with
// [ServiceFromHost].
package delegation
