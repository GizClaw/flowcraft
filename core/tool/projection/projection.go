package projection

import (
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Projection is the process-wide visibility machinery: one catalog, one exposure
// policy, and the search index every session shares.
//
// It is not a session store. A session's discovery state — which tools its model has
// found, which it used last round — belongs to that session, so a host asks for a
// [View] and keeps it next to the session it serves. Keeping the views here as well
// would mean a second session table that had to be evicted in lockstep with the
// first, and two tables that must agree are one table with a race.
type Projection struct {
	catalog tool.Catalog
	policy  Policy
	// searchCache is the BM25 index every view shares. It is keyed by the
	// catalog's definition epoch, so an MCP tool list change invalidates it without
	// any view having to notice.
	searchCache *searchIndexCache
}

// New builds the projection over catalog with policy.
//
// policy is normalized and validated here, so a caller may pass a partially filled
// document and learn about a contradiction at construction rather than at the first
// turn. catalog must be non-nil.
func New(catalog tool.Catalog, policy Policy) (*Projection, error) {
	if catalog == nil {
		panic("projection.New: catalog is nil")
	}
	normalized := normalizePolicy(policy)
	if err := normalized.Validate(); err != nil {
		return nil, err
	}
	return &Projection{
		catalog:     catalog,
		policy:      normalized,
		searchCache: &searchIndexCache{},
	}, nil
}

// Catalog is the read surface the projection sees. A host keeps it to report what was
// wired; execution does not go through here.
func (p *Projection) Catalog() tool.Catalog { return p.catalog }

// Policy is the normalized policy in force.
func (p *Projection) Policy() Policy { return p.policy }

// NewView creates the discovery view for one session.
//
// A view starts with nothing discovered: the model sees what the policy always
// exposes, and everything else arrives through tool_search or through a call the model
// already made.
func (p *Projection) NewView() *View {
	return &View{
		catalog:     p.catalog,
		projection:  p,
		policy:      p.policy,
		st:          *newViewState(),
		searchCache: p.searchCache,
	}
}

// Static returns the view a process without discovery hands out: every tool visible,
// no state, and tool_search unavailable.
//
// It is a separate implementation rather than a projection with a permissive policy
// because it genuinely has no state: nothing is discovered, nothing expires, and
// Definitions is the catalog's own list.
func Static(catalog tool.Catalog) tool.View { return staticView{catalog: catalog} }

// staticView is the non-dynamic view: every tool visible, no state.
type staticView struct {
	catalog tool.Catalog
}

func (s staticView) Definitions() []message.ToolDefinition { return s.catalog.Definitions() }

func (s staticView) Visible(name string) bool {
	_, ok := s.catalog.Get(name)
	return ok
}

func (s staticView) RecordCall(message.ToolCall) {}
