package policy

import "github.com/GizClaw/flowcraft/core/model"

// Request is what a caller asks for when opening a session: a selection inside
// the agent's grant, never a replacement for it.
//
// It deliberately has no path field. A remote peer (an A2A peer, an HTTP client)
// is untrusted input, and the cheapest way to make sure untrusted input cannot
// choose a boundary is for the boundary to have no representation here. The host
// supplies files, network and sandbox out of band, where the trust boundary
// actually is; everything a caller can say is limited to this struct.
type Request struct {
	// Models selects a subset of the granted models. Empty keeps the grant's
	// set.
	Models []model.ModelRef `json:"models,omitempty" yaml:"models,omitempty"`
	// Tools narrows the granted tool set. Nil keeps the grant's set.
	Tools *ToolSet `json:"tools,omitempty" yaml:"tools,omitempty"`
	// Budget narrows the per-round definition budget. Nil keeps the grant's.
	Budget *Budget `json:"budget,omitempty" yaml:"budget,omitempty"`
}

// IsZero reports whether the request asks for nothing narrower than the grant.
func (r Request) IsZero() bool {
	return len(r.Models) == 0 && r.Tools == nil && r.Budget == nil
}
