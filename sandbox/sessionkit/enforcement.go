package sessionkit

import "github.com/GizClaw/flowcraft/core/sandbox"

// Enforcement fills in the resource caps this package's group sampler can
// enforce and leaves every other dimension as the caller declared it.
//
// It exists so "can this host cap memory and cpu" has one answer: the answer
// comes from the sampler that would do the enforcing, not from a backend
// guessing. A backend that enforces caps some other way must not call it — the
// windows backend uses job objects, so it reports MemoryCap / CPUCap itself
// rather than reporting the sampler's availability.
//
// The env allow-list is not filled in here: every backend does its own env
// filtering, so claiming it on their behalf would be a promise this package
// cannot keep.
func Enforcement(policy sandbox.Enforcement) sandbox.Enforcement {
	caps := groupCapsAvailable()
	policy.MemoryCap = caps
	policy.CPUCap = caps
	return policy
}
