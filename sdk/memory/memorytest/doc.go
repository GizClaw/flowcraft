// Package memorytest provides reusable conformance suites for
// memory implementations.
//
// The package is the black-box contract for *memory.Runtime impls:
// each Run* function drives a *memory.Runtime wired with the
// impl under test through the documented surface and asserts the
// properties the kernel relies on (Scope partitioning, Compile
// ledger completeness, IdempotencyKey dedup, Cursor pagination,
// opaque Source/NextCursor, …).
//
// # Usage
//
// A test file in an impl package typically looks like:
//
//	func TestConformance(t *testing.T) {
//	    memorytest.RunScope(t, memorytest.ScopeSuite{...})
//	    memorytest.RunAppend(t, memorytest.AppendSuite{
//	        Spec: memory.Spec{RuntimeID: "test"},
//	        BuildRuntime: func(t *testing.T) *memory.Runtime {
//	            return mustBuild(t)
//	        },
//	        SampleScope: memory.Scope{RuntimeID: "test", UserID: "u1"},
//	    })
//	}
//
// # Kernel-level tests
//
// Tests for the kernel itself (Compile invariants, scope
// validation, ledger enforcement, NotConfigured rejection)
// live in sdk/memory's own _test.go files. memorytest is for
// what an impl must do given a working kernel.
//
// # NoopRuntime
//
// NoopRuntime is a real impl that satisfies every op with the
// zero value. memorytest.RunNoop runs the full contract
// against NoopRuntime so an impl that has trouble passing
// RunNoop has a problem with the kernel, not with the
// individual tests.
package memorytest
