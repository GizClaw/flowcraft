package agent

// Agent is the assembled, runnable form of a [Definition]: identity,
// card, tool allow-list, the constructed engine, and the attached
// lifecycle hooks. The deployment layer builds it from a Definition;
// hosts and runtimes consume it. The hook slices map 1:1 to the four
// slots declared on Definition.
//
// [Execute] can be driven either by [Agent.Engine] (assembled agents)
// or by an engine passed explicitly to Execute (which wins).
type Agent struct {
	ID       string      `json:"id"`
	Card     AgentCard   `json:"card,omitzero"`
	Tools    []string    `json:"tools,omitempty"`
	Policy   Policy      `json:"-"`
	Engine   Engine      `json:"-"`
	Prepare  []Preparer  `json:"-"`
	Observe  []Observer  `json:"-"`
	Referees []Referee   `json:"-"`
	Commit   []Committer `json:"-"`
}
