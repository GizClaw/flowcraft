package vessel

// Phase enumerates the [Captain] lifecycle states. Phase transitions
// are linear within a single launch:
//
//	Pending  ─Launch─►  Running  ─Drain─►  Draining ─done─►  Stopped
//	   │                   │                   │
//	   │ Launch fails      │ Stop              │ Stop
//	   ▼                   ▼                   ▼
//	 Failed              Stopping  ─done─►  Stopped
//
// A vessel that never Launched stays in Pending. After Launch
// succeeds the vessel is Running until either Drain (cooperative)
// or Stop (impatient) starts the teardown. Stopped is terminal in
// PR1; PR5 adds the restart loop that re-enters Pending → Running.
type Phase string

const (
	// PhasePending is the initial state. The vessel has been
	// constructed but Launch has not been called.
	PhasePending Phase = "pending"

	// PhaseRunning means the vessel is admitting Submit / Call
	// requests and dispatching them to agents.
	PhaseRunning Phase = "running"

	// PhaseDraining means new Submit / Call requests are rejected
	// (errdefs.NotAvailable) but in-flight runs are allowed to
	// complete naturally.
	PhaseDraining Phase = "draining"

	// PhaseStopping means new requests are rejected AND in-flight
	// runs are being cancelled. PR1 implements Stop as a hard
	// cancel: ctx propagation aborts whatever the agent is doing.
	PhaseStopping Phase = "stopping"

	// PhaseStopped means the vessel has fully shut down. All
	// goroutines have exited, the bus is closed, and Submit /
	// Call permanently return errdefs.NotAvailable.
	PhaseStopped Phase = "stopped"

	// PhaseFailed means Launch encountered a fatal error before
	// the vessel could enter Running. Inspect the value returned
	// from Launch for the cause.
	PhaseFailed Phase = "failed"
)

// IsTerminal reports whether p is a phase from which no further
// transitions are possible (in PR1: Stopped or Failed). The Captain
// uses this to short-circuit double-Stop and double-Drain calls.
func (p Phase) IsTerminal() bool {
	return p == PhaseStopped || p == PhaseFailed
}

// AcceptsRequests reports whether the phase admits new Submit /
// Call requests. Only PhaseRunning does.
func (p Phase) AcceptsRequests() bool { return p == PhaseRunning }
