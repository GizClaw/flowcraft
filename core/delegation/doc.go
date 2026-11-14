// Package delegation assigns one agent's work to another deployed agent.
//
// Its unit is a Target: an agent name resolved through Sessions. The package
// knows nothing about deploy documents, runtimes or A2A: a host adapts its
// manager to Sessions, and the tools it builds are injected into the calling
// agent's runtime.
//
// The first slice is synchronous and in-process. Async jobs and a durable
// store extend the same Job/Policy model later; they do not change the
// Sessions contract.
package delegation
