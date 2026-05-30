// Package contract provides a backend-agnostic test suite that any
// retrieval.Index implementation MUST pass.
//
// Usage from an adapter package:
//
//	func TestMemoryIndexContract(t *testing.T) {
//	    contract.Run(t, func(_ *testing.T) (retrieval.Index, func()) {
//	        return memory.New(), func() {}
//	    })
//	}
package contract
