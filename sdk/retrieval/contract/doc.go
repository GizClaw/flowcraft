// Package contract provides a backend-agnostic test suite that any
// retrieval.Index implementation MUST pass.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/retrieval/contract instead.
// This package will be removed in v0.5.0.
//
// Usage from an adapter package:
//
//	func TestMemoryIndexContract(t *testing.T) {
//	    contract.Run(t, func(_ *testing.T) (retrieval.Index, func()) {
//	        return memory.New(), func() {}
//	    })
//	}
package contract
