package v1alpha1

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Agent is the wire form of a single vessel agent. Only the fields
// the daemon needs to differentiate at startup live here; rich
// engine config (system prompt, model overrides, retries) is
// nested under Engine.Config and treated opaquely by the apispec
// layer — the engine factory referenced by Engine.Ref is the only
// thing that knows the config schema for that engine.
type Agent struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AgentSpec `json:"spec" yaml:"spec"`
}

// AgentSpec carries the per-agent configuration.
type AgentSpec struct {
	// Card carries the AgentCard subset the runtime exposes to
	// A2A discovery. Optional; when absent the resolver synthesises
	// a card from the agent name.
	Card *AgentCard `json:"card,omitempty" yaml:"card,omitempty"`

	// HistoryAccess selects the agent's read/write window onto
	// the shared history. Empty resolves to "read_write" when the
	// vessel has a History configured, otherwise "none".
	HistoryAccess string `json:"historyAccess,omitempty" yaml:"historyAccess,omitempty"` // none | read_only | read_write

	// Dispatcher = true auto-installs kanban_submit and
	// task_context tools onto the agent's allow-list and routes
	// terminal callbacks into its history. Requires
	// VesselSpec.Kanban != nil; the resolver enforces that.
	Dispatcher bool `json:"dispatcher,omitempty" yaml:"dispatcher,omitempty"`

	// ProducerChain caps nested dispatch depth. 0 falls back to
	// VesselSpec.Kanban.MaxProducerChain.
	ProducerChain int `json:"producerChain,omitempty" yaml:"producerChain,omitempty"`

	// Sidecar marks an agent that is bus-triggered rather than
	// Submit-triggered. SubscribeTo must be set whenever Sidecar
	// is true (and must be empty otherwise).
	Sidecar     bool   `json:"sidecar,omitempty" yaml:"sidecar,omitempty"`
	SubscribeTo string `json:"subscribeTo,omitempty" yaml:"subscribeTo,omitempty"`

	// Tools is the allow-list applied to the engine's tool
	// resolution. Dispatcher auto-injection adds to this list at
	// resolve time without modifying the user's config.
	Tools []string `json:"tools,omitempty" yaml:"tools,omitempty"`

	// Engine selects the EngineFactory and supplies its config.
	// Required: every agent needs a runnable engine.
	Engine AgentEngine `json:"engine" yaml:"engine"`
}

// AgentCard mirrors the agent.AgentCard wire fields the resolver
// can populate. We do NOT reuse the SDK type directly so wire
// changes here are independent of the runtime card evolution.
type AgentCard struct {
	Name        string   `json:"name,omitempty" yaml:"name,omitempty"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
	Skills      []string `json:"skills,omitempty" yaml:"skills,omitempty"`
}

// AgentEngine is the engine ref + opaque config map. The factory
// registered under Ref decides what the Config keys mean. We use
// map[string]any rather than a typed struct because the engine
// catalog is open-ended (graph-llm, graph-recall, custom forks,
// future plugins) and forcing a closed schema here would defeat
// the catalog's purpose.
type AgentEngine struct {
	Ref    string         `json:"ref" yaml:"ref"`
	Config map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

func (a Agent) GetTypeMeta() TypeMeta     { return a.TypeMeta }
func (a Agent) GetObjectMeta() ObjectMeta { return a.ObjectMeta }

// Validate runs the wire-form schema checks. Engine config schema
// validation is delegated to the engine factory at resolve time;
// validating it here would mean apispec needs to know every
// factory, defeating the catalog abstraction.
func (a Agent) Validate() error {
	if err := a.ObjectMeta.Validate(KindAgent); err != nil {
		return err
	}
	if a.TypeMeta.APIVersion != APIVersion {
		return errdefs.Validationf("vesseld Agent %q: apiVersion %q != %q", a.Name, a.TypeMeta.APIVersion, APIVersion)
	}
	if a.TypeMeta.Kind != KindAgent {
		return errdefs.Validationf("vesseld Agent %q: kind %q != %q", a.Name, a.TypeMeta.Kind, KindAgent)
	}
	switch a.Spec.HistoryAccess {
	case "", "none", "read_only", "read_write":
	default:
		return errdefs.Validationf("vesseld Agent %q: spec.historyAccess %q invalid (want none|read_only|read_write)", a.Name, a.Spec.HistoryAccess)
	}
	if a.Spec.Sidecar && a.Spec.SubscribeTo == "" {
		return errdefs.Validationf("vesseld Agent %q: spec.subscribeTo is required when spec.sidecar=true", a.Name)
	}
	if !a.Spec.Sidecar && a.Spec.SubscribeTo != "" {
		return errdefs.Validationf("vesseld Agent %q: spec.subscribeTo set without spec.sidecar=true", a.Name)
	}
	if a.Spec.ProducerChain < 0 {
		return errdefs.Validationf("vesseld Agent %q: spec.producerChain must be >= 0", a.Name)
	}
	if a.Spec.Engine.Ref == "" {
		return errdefs.Validationf("vesseld Agent %q: spec.engine.ref is required", a.Name)
	}
	return nil
}
