package v1alpha1

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Vessel describes one Vessel hosted by the daemon. The schema is
// the wire-form of spec.Spec, expressed in declarative refs:
// agents are referenced by name (resolved against Agent docs in
// the same vessel sub-folder), the history store / probes are
// referenced by name (resolved against shared/ docs).
//
// Why we do not embed Agent definitions inline: a real config
// repo will have many agents, and one agent per file is far easier
// to review and rebase than a single 500-line vessel.yaml.
type Vessel struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       VesselSpec `json:"spec" yaml:"spec"`
}

// VesselSpec is the per-vessel configuration. Field semantics map
// 1:1 onto spec.Spec at resolve time; the only divergence is
// the indirection through name refs.
type VesselSpec struct {
	// Agents lists the names of Agent documents this vessel
	// includes. Order is preserved; the first non-Sidecar agent
	// is the "primary" agent (same convention as vesselspec).
	Agents []string `json:"agents" yaml:"agents"`

	// History optionally points at a HistoryStore in shared/. nil
	// means "no shared history"; agents that opt into history
	// access then have nothing to load and Submit succeeds with
	// empty seeded transcripts.
	History *NamedRef `json:"history,omitempty" yaml:"history,omitempty"`

	// Kanban opts into the agent-as-tool subsystem. nil disables
	// it (Agent.dispatcher must remain false in that case).
	Kanban *VesselKanban `json:"kanban,omitempty" yaml:"kanban,omitempty"`

	// Resources copies spec.Resources verbatim.
	Resources VesselResources `json:"resources,omitempty" yaml:"resources,omitempty"`

	// Restart copies spec.Restart verbatim. Mode "" is the
	// resolver-applied default ("never").
	Restart VesselRestart `json:"restart,omitempty" yaml:"restart,omitempty"`

	// Probes references shared/ probes by name plus the loop
	// timing parameters. Resolution maps liveness names to
	// spec.Probe instances.
	Probes *VesselProbes `json:"probes,omitempty" yaml:"probes,omitempty"`
}

// NamedRef is a tiny "{ref: name}" wrapper. Used wherever a field
// can either be a single ref or absent. Wrapping keeps the YAML
// uniform with multi-field references like Agent.engine.
type NamedRef struct {
	Ref string `json:"ref" yaml:"ref"`
}

// VesselKanban mirrors spec.Kanban.
type VesselKanban struct {
	MaxPendingTasks    int `json:"maxPendingTasks,omitempty" yaml:"maxPendingTasks,omitempty"`
	MaxProducerChain   int `json:"maxProducerChain,omitempty" yaml:"maxProducerChain,omitempty"`
	CallbackMaxSummary int `json:"callbackMaxSummary,omitempty" yaml:"callbackMaxSummary,omitempty"`
}

// VesselResources mirrors spec.Resources.
type VesselResources struct {
	MaxConcurrentRuns int           `json:"maxConcurrentRuns,omitempty" yaml:"maxConcurrentRuns,omitempty"`
	TurnTimeout       time.Duration `json:"turnTimeout,omitempty" yaml:"turnTimeout,omitempty"`
	// MaxTokensPerTurn caps the per-Run token total reported via
	// engine.UsageReporter. 0 = unlimited.
	MaxTokensPerTurn int64 `json:"maxTokensPerTurn,omitempty" yaml:"maxTokensPerTurn,omitempty"`
	// MaxTokensPerHour caps the vessel-wide rolling-hour total
	// reported via engine.UsageReporter. 0 = unlimited.
	MaxTokensPerHour int64 `json:"maxTokensPerHour,omitempty" yaml:"maxTokensPerHour,omitempty"`
}

// VesselRestart mirrors spec.Restart.
type VesselRestart struct {
	Mode        string        `json:"mode,omitempty" yaml:"mode,omitempty"` // never | on_failure
	MaxRestarts int           `json:"maxRestarts,omitempty" yaml:"maxRestarts,omitempty"`
	BackoffInit time.Duration `json:"backoffInit,omitempty" yaml:"backoffInit,omitempty"`
	BackoffMax  time.Duration `json:"backoffMax,omitempty" yaml:"backoffMax,omitempty"`
}

// VesselProbes references probes by name plus the loop timings.
// Liveness is a list of name refs; the resolver looks each up in
// the shared inventory.
type VesselProbes struct {
	Liveness         []string      `json:"liveness,omitempty" yaml:"liveness,omitempty"`
	Interval         time.Duration `json:"interval,omitempty" yaml:"interval,omitempty"`
	Timeout          time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	FailureThreshold int           `json:"failureThreshold,omitempty" yaml:"failureThreshold,omitempty"`
}

func (v Vessel) GetTypeMeta() TypeMeta     { return v.TypeMeta }
func (v Vessel) GetObjectMeta() ObjectMeta { return v.ObjectMeta }

// Validate runs the wire-form checks. Cross-document reference
// validation (does the named History exist, do the named Agents
// exist) is the resolver's job, because it needs the loaded set
// across all documents.
func (v Vessel) Validate() error {
	if err := v.ObjectMeta.Validate(KindVessel); err != nil {
		return err
	}
	if v.TypeMeta.APIVersion != APIVersion {
		return errdefs.Validationf("vesseld Vessel %q: apiVersion %q != %q", v.Name, v.TypeMeta.APIVersion, APIVersion)
	}
	if v.TypeMeta.Kind != KindVessel {
		return errdefs.Validationf("vesseld Vessel %q: kind %q != %q", v.Name, v.TypeMeta.Kind, KindVessel)
	}
	if len(v.Spec.Agents) == 0 {
		return errdefs.Validationf("vesseld Vessel %q: spec.agents must contain at least one entry", v.Name)
	}
	for i, ref := range v.Spec.Agents {
		if ref == "" {
			return errdefs.Validationf("vesseld Vessel %q: spec.agents[%d] is empty", v.Name, i)
		}
	}
	if v.Spec.History != nil && v.Spec.History.Ref == "" {
		return errdefs.Validationf("vesseld Vessel %q: spec.history.ref is empty", v.Name)
	}
	if v.Spec.Resources.MaxConcurrentRuns < 0 {
		return errdefs.Validationf("vesseld Vessel %q: spec.resources.maxConcurrentRuns must be >= 0", v.Name)
	}
	if v.Spec.Resources.TurnTimeout < 0 {
		return errdefs.Validationf("vesseld Vessel %q: spec.resources.turnTimeout must be >= 0", v.Name)
	}
	if v.Spec.Resources.MaxTokensPerTurn < 0 {
		return errdefs.Validationf("vesseld Vessel %q: spec.resources.maxTokensPerTurn must be >= 0", v.Name)
	}
	if v.Spec.Resources.MaxTokensPerHour < 0 {
		return errdefs.Validationf("vesseld Vessel %q: spec.resources.maxTokensPerHour must be >= 0", v.Name)
	}
	switch v.Spec.Restart.Mode {
	case "", "never", "on_failure":
	default:
		return errdefs.Validationf("vesseld Vessel %q: spec.restart.mode %q invalid (want never|on_failure)", v.Name, v.Spec.Restart.Mode)
	}
	if v.Spec.Restart.MaxRestarts < 0 || v.Spec.Restart.BackoffInit < 0 || v.Spec.Restart.BackoffMax < 0 {
		return errdefs.Validationf("vesseld Vessel %q: spec.restart numeric fields must be >= 0", v.Name)
	}
	if v.Spec.Kanban != nil {
		if v.Spec.Kanban.MaxPendingTasks < 0 || v.Spec.Kanban.MaxProducerChain < 0 || v.Spec.Kanban.CallbackMaxSummary < 0 {
			return errdefs.Validationf("vesseld Vessel %q: spec.kanban fields must be >= 0", v.Name)
		}
	}
	if v.Spec.Probes != nil {
		if v.Spec.Probes.Interval < 0 || v.Spec.Probes.Timeout < 0 || v.Spec.Probes.FailureThreshold < 0 {
			return errdefs.Validationf("vesseld Vessel %q: spec.probes timing fields must be >= 0", v.Name)
		}
		for i, ref := range v.Spec.Probes.Liveness {
			if ref == "" {
				return errdefs.Validationf("vesseld Vessel %q: spec.probes.liveness[%d] is empty", v.Name, i)
			}
		}
	}
	return nil
}
