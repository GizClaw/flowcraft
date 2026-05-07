package spec

import (
	"context"
	"time"
)

// Probe is the contract for a health check the Captain invokes in
// the background. Custom probes implement this interface in user
// code; built-in probes (LLM-reachable, tool-reachable, …) live
// alongside the vessel runtime.
type Probe interface {
	// Name identifies the probe in observability and probe-result
	// reporting. Should be stable across restarts (e.g.
	// "llm-reachable", "tool-reachable").
	Name() string

	// Check runs the probe. Implementations MUST honour ctx for
	// cancellation — the Captain bounds Check with a deadline
	// derived from Probes.Timeout. Returning Healthy=false is
	// normal and counts toward the configured FailureThreshold;
	// returning a non-nil error is treated identically (the error
	// message becomes the failure Reason).
	Check(ctx context.Context) (ProbeResult, error)
}

// ProbeResult is the per-Check outcome surfaced to the Captain.
type ProbeResult struct {
	// Healthy is the binary verdict; consecutive false results
	// drive phase transitions per [Restart].
	Healthy bool

	// Latency captures the wall-clock duration of Check, useful
	// for SLA dashboards.
	Latency time.Duration

	// Reason explains the verdict in one human-readable line. For
	// healthy results it MAY be empty; for unhealthy results it
	// SHOULD describe the failure ("llm: 503 service unavailable",
	// "tool registry empty", …).
	Reason string

	// Detail carries optional structured context about the result
	// (per-tool latencies, the upstream HTTP status, …). The
	// Captain copies it onto emitted probe-failure envelopes.
	Detail map[string]any
}

// Probes configures the per-vessel probe loop. The zero value is
// not used directly; populate Liveness with at least one [Probe] and
// optionally tune the cadence knobs.
type Probes struct {
	// Liveness probes determine whether the vessel can recover
	// itself; consecutive failures up to FailureThreshold flip the
	// Captain to PhaseFailed, where [Restart] dictates what
	// happens next.
	Liveness []Probe `json:"-" yaml:"-"`

	// Interval is the cadence between successive probe rounds.
	// Defaults to 30s when zero.
	Interval time.Duration `json:"interval,omitempty" yaml:"interval,omitempty"`

	// Timeout bounds each Check call. Defaults to 5s when zero.
	Timeout time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// FailureThreshold is the number of consecutive failures that
	// flips a probe to "down". Defaults to 3.
	FailureThreshold int `json:"failure_threshold,omitempty" yaml:"failure_threshold,omitempty"`
}

// ProbeFunc adapts a plain function into a [Probe]. The Label is
// captured at construction so adapter chains keep observability
// metadata intact.
type ProbeFunc struct {
	Label string
	Fn    func(ctx context.Context) (ProbeResult, error)
}

// Name satisfies Probe.
func (p ProbeFunc) Name() string { return p.Label }

// Check satisfies Probe. A nil Fn always reports Healthy=true so
// callers can use a bare ProbeFunc{Label: "..."} as a placeholder
// during wiring.
func (p ProbeFunc) Check(ctx context.Context) (ProbeResult, error) {
	if p.Fn == nil {
		return ProbeResult{Healthy: true}, nil
	}
	return p.Fn(ctx)
}
