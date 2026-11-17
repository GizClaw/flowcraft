// Package backends is the opt-in table of the sandbox backends core ships.
//
// It exists so a host does not have to re-derive each backend's construction
// options, and it is opt-in on purpose: core/runtime imports none of these, so a
// host that registers only [Local] links only the local runner. A host that wants
// everything registers [All].
//
// Every factory reads the section of the deployment document it owns —
// sandbox.bwrap, sandbox.seatbelt, sandbox.windows, sandbox.remote — plus the
// shared build context (directory, writable paths, limits, journal).
package backends

import "github.com/GizClaw/flowcraft/core/runtime"

// All returns every backend core ships, keyed by the value a deployment writes in
// sandbox.backend.
func All() map[string]runtime.SandboxFactory {
	return map[string]runtime.SandboxFactory{
		runtime.BackendLocal:    Local(),
		runtime.BackendBwrap:    Bwrap(),
		runtime.BackendSeatbelt: Seatbelt(),
		runtime.BackendWindows:  Windows(),
		runtime.BackendRemote:   Remote(),
	}
}
