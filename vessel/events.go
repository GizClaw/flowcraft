package vessel

import "github.com/GizClaw/flowcraft/sdk/event"

// SubjectPhaseChanged is the event subject the Captain publishes
// every time its [Phase] transitions. Subscribers built atop the
// vessel's bus (dashboards, autoscalers, log shippers) use this to
// observe lifecycle changes without polling Captain.Phase.
//
// Payload type: [PhaseChangedPayload].
const SubjectPhaseChanged event.Subject = "vessel.phase.changed"

// SubjectProbeFailed is the event subject the Captain publishes
// every time a probe round records a failure (Healthy=false or
// non-nil error). One envelope per failing probe per round —
// recovery (Healthy=true after a failure streak) is observable via
// the absence of envelopes; subscribers wanting an explicit signal
// can correlate against [SubjectPhaseChanged].
//
// Payload type: [ProbeFailedPayload].
const SubjectProbeFailed event.Subject = "vessel.probe.failed"

// PhaseChangedPayload describes one [Phase] transition. The Captain
// publishes it on [SubjectPhaseChanged] with the vessel id stamped
// onto Envelope.Source so consumers can fan-in by vessel.
type PhaseChangedPayload struct {
	VesselID string `json:"vessel_id"`
	From     Phase  `json:"from"`
	To       Phase  `json:"to"`
	// Reason is a short human-readable explanation. Set when the
	// transition was driven by a non-trivial cause (probe failure,
	// restart attempt, fatal error). Empty for nominal transitions
	// (Pending → Running on Launch, Running → Stopped on a clean
	// Drain).
	Reason string `json:"reason,omitempty"`
}

// ProbeFailedPayload describes one failing probe round.
type ProbeFailedPayload struct {
	VesselID string `json:"vessel_id"`
	// Probe is the [spec.Probe.Name] of the failing probe.
	Probe string `json:"probe"`
	// Reason mirrors [spec.ProbeResult.Reason] — the human-
	// readable failure cause. Empty when the probe returned a
	// non-nil error (in which case Error carries it).
	Reason string `json:"reason,omitempty"`
	// Error is set when Check itself returned an error; mutually
	// exclusive with a Healthy=false ProbeResult.
	Error string `json:"error,omitempty"`
	// Streak is the consecutive-failure count for this probe,
	// including the current round. The Captain trips into
	// PhaseFailed when Streak >= configured FailureThreshold.
	Streak int `json:"streak"`
	// Detail mirrors [spec.ProbeResult.Detail].
	Detail map[string]any `json:"detail,omitempty"`
}
