package workflow

// Agent describes identity and execution strategy for one logical agent.
type Agent interface {
	ID() string
	Card() AgentCard
	Strategy() Strategy
	Tools() []string
}

// AgentCard describes capabilities for discovery (e.g. A2A).
type AgentCard struct {
	Name         string
	Description  string
	Skills       []Skill
	InputModes   []string
	OutputModes  []string
	Capabilities AgentCapabilities
}

// Skill is a lightweight skill declaration on an AgentCard.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// AgentCapabilities declares optional runtime features.
type AgentCapabilities struct {
	Streaming        bool `json:"streaming,omitempty"`
	PushNotification bool `json:"push_notification,omitempty"`
	StateTransition  bool `json:"state_transition,omitempty"`
}

type simpleAgent struct {
	id       string
	card     AgentCard
	strategy Strategy
	tools    []string
}

func (a *simpleAgent) ID() string         { return a.id }
func (a *simpleAgent) Card() AgentCard    { return a.card }
func (a *simpleAgent) Strategy() Strategy { return a.strategy }
func (a *simpleAgent) Tools() []string    { return a.tools }

// AgentOption configures NewAgent.
type AgentOption func(*simpleAgent)

// WithAgentDescription sets the card description.
func WithAgentDescription(desc string) AgentOption {
	return func(a *simpleAgent) { a.card.Description = desc }
}

// WithAgentTools sets tool names exposed to the runtime.
func WithAgentTools(tools []string) AgentOption {
	return func(a *simpleAgent) { a.tools = tools }
}

// NewAgent constructs a basic Agent backed by the given Strategy.
func NewAgent(id string, strategy Strategy, opts ...AgentOption) Agent {
	a := &simpleAgent{
		id:       id,
		strategy: strategy,
		card: AgentCard{
			Name: id,
		},
	}
	for _, o := range opts {
		o(a)
	}
	return a
}
