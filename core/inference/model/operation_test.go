package model

import (
	"testing"
)

// TestOperationVocabulary pins that the vocabulary enumerates only workloads
// with a request/response surface. The "realtime" value the original
// declaration reserved is not part of it: nothing produces or consumes it,
// and providers are not allowed to advertise it. core/inference inherits this
// vocabulary through its aliases, so deployments can no longer declare it
// either.
func TestOperationVocabulary(t *testing.T) {
	if len(Operations()) == 0 {
		t.Fatal("Operations must declare at least one workload")
	}
	seen := make(map[Operation]bool, len(Operations()))
	for _, operation := range Operations() {
		if seen[operation] {
			t.Errorf("%q is enumerated twice", operation)
		}
		seen[operation] = true
		if err := operation.Validate(); err != nil {
			t.Errorf("%q is enumerated but does not validate: %v", operation, err)
		}
	}
	// Validate is derived from Operations, so anything outside the enumeration
	// is rejected — including the reserved "realtime" value the original
	// declaration carried.
	if err := Operation("realtime").Validate(); err == nil {
		t.Error(`"realtime" must not validate: the surface does not exist yet`)
	}
	if err := Operation("").Validate(); err == nil {
		t.Error("empty operation must not validate")
	}
}
