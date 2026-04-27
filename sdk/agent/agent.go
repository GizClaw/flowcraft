package agent

// Agent is the application-layer description of one logical agent. It
// is a plain data struct, NOT a "live object" that knows how to run
// itself: execution is performed by passing an [engine.Engine] to
// [Run] alongside the Agent. The same Agent value can therefore be
// driven by different engines without re-construction.
//
// This is the central distinction from sdk/workflow.Agent, which
// internally owned a Strategy and so could only execute one way.
type Agent struct {
	// ID is the stable identifier for this agent. It flows into
	// telemetry, history conversation keys, and any A2A federation
	// envelope. MUST be non-empty.
	ID string `json:"id"`

	// Card describes the agent's capabilities for discovery (A2A,
	// dashboards, …). Optional.
	Card AgentCard `json:"card,omitempty"`

	// Tools is the list of tool ids the agent is permitted to call.
	// The engine looks tools up by id in its dependency container at
	// run time; this list is the policy gate, not the wiring.
	Tools []string `json:"tools,omitempty"`

	// Observers are agent-scoped lifecycle observers. They fire on
	// every [Run] of this agent value, before any observers added
	// via [WithObserver] for the specific call. JSON-skipped because
	// observers carry runtime state (channels, stores, …) that does
	// not round-trip through serialisation.
	Observers []Observer `json:"-"`

	// Deciders are agent-scoped decision hooks (see [Decider]). They
	// run before any Decider added via [WithDecider] for the
	// specific call. Same JSON-skip rationale as Observers.
	Deciders []Decider `json:"-"`
}

// AgentCard describes an agent's capabilities for discovery. Field
// names and JSON tags are a *proper subset* of the A2A AgentCard
// specification: every field marshals to the exact key A2A readers
// expect, and AgentCard never collides with an A2A field by using a
// different name.
//
// What is intentionally NOT here:
//
//   - url / version / provider / documentationUrl / authentication —
//     these belong to "how the agent is exposed as a service", a
//     concern owned by sdk/a2a (when added). The intended layering
//     is that sdk/a2a's expose-time card embeds [AgentCard] and adds
//     the deployment fields:
//
//     type Card struct {
//     agent.AgentCard
//     URL              string `json:"url"`
//     Version          string `json:"version"`
//     Provider         *Provider `json:"provider,omitempty"`
//     Authentication   *Authentication `json:"authentication,omitempty"`
//     DocumentationURL string `json:"documentationUrl,omitempty"`
//     }
//
//     This keeps sdk/agent's runtime-identity surface stable when the
//     A2A spec evolves its deployment metadata.
//
// Reference: https://agent2agent.info/docs/concepts/agentcard/
type AgentCard struct {
	// Name is a human-readable name for the agent.
	Name string `json:"name"`

	// Description explains what the agent does.
	Description string `json:"description,omitempty"`

	// Skills enumerates the capability units the agent can perform.
	Skills []Skill `json:"skills,omitempty"`

	// DefaultInputModes lists MIME types the agent accepts when a
	// skill does not override them. Per the A2A spec, this field is
	// keyed "defaultInputModes" — singular "inputModes" lives on
	// individual skills.
	DefaultInputModes []string `json:"defaultInputModes,omitempty"`

	// DefaultOutputModes lists MIME types the agent emits when a
	// skill does not override them.
	DefaultOutputModes []string `json:"defaultOutputModes,omitempty"`

	// Capabilities declares which optional A2A protocol features the
	// agent supports.
	Capabilities AgentCapabilities `json:"capabilities,omitempty"`
}

// Skill is a single capability unit declared on an [AgentCard]. Field
// names mirror A2A's skill object so cards round-trip cleanly through
// /.well-known/agent-card.json.
type Skill struct {
	// ID is the unique identifier for this skill within the agent.
	ID string `json:"id"`

	// Name is a human-readable label for the skill.
	Name string `json:"name"`

	// Description explains what the skill does.
	Description string `json:"description,omitempty"`

	// Tags categorises the skill (e.g. "cooking", "support").
	Tags []string `json:"tags,omitempty"`

	// Examples lists illustrative prompts the skill can handle.
	Examples []string `json:"examples,omitempty"`

	// InputModes overrides AgentCard.DefaultInputModes for this
	// specific skill. Empty means "use the agent default".
	InputModes []string `json:"inputModes,omitempty"`

	// OutputModes overrides AgentCard.DefaultOutputModes for this
	// specific skill. Empty means "use the agent default".
	OutputModes []string `json:"outputModes,omitempty"`
}

// AgentCapabilities declares which optional A2A features the agent
// supports. JSON keys exactly match the A2A spec — note the plural
// PushNotifications and the longer StateTransitionHistory.
type AgentCapabilities struct {
	// Streaming reports whether the agent emits server-sent events
	// during a turn.
	Streaming bool `json:"streaming,omitempty"`

	// PushNotifications reports whether the agent can push update
	// notifications back to the client.
	PushNotifications bool `json:"pushNotifications,omitempty"`

	// StateTransitionHistory reports whether the agent exposes its
	// task state-transition history.
	StateTransitionHistory bool `json:"stateTransitionHistory,omitempty"`
}
