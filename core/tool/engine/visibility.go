package engine

import (
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/message"
)

// candidate couples one registry tool with its exposure and definition
// for the visibility computation.
type candidate struct {
	name string
	def  message.ToolDefinition
	exp  Exposure
}

// visibleOrder applies the visibility algorithm and returns the indexes into
// cands that reach the model this round, sorted by name for stable output. It
// is the one implementation: Definitions maps the indexes onto definitions and
// Discover onto names, so the exposure report can never drift from the
// definitions that are actually sent. The computation is pure: it never mutates
// the candidates, the state or the policy.
//
// It returns indexes rather than candidates because the candidate slice is
// cached per catalog epoch and the per-round result is the definitions to send:
// copying every included candidate twice, once per round, was the bulk of the
// round's allocation.
func visibleOrder(cands []candidate, st stateSnapshot, policy Policy) []int32 {
	budget := policy.Budget
	order := make([]int32, 0, len(cands))
	for i, c := range cands {
		if include(c, st, policy) {
			order = append(order, int32(i))
		}
	}

	// Deterministic pruning order: exposure rank, RequiredByName,
	// discovery-pool recency, then name. Same-round pool entries share
	// a lastUse, so they fall back to discovery order — the first
	// (best-ranked) hit of a tool_search batch wins the cut.
	slices.SortStableFunc(order, func(a, b int32) int {
		switch {
		case lessPriority(cands[a], cands[b], st):
			return -1
		case lessPriority(cands[b], cands[a], st):
			return 1
		default:
			return 0
		}
	})
	if len(order) > budget.MaxDefinitions {
		order = order[:budget.MaxDefinitions]
	}

	// Filter in place: the index being read is at or after the one being
	// written, so the byte-budget skip stays safe while compacting.
	kept := 0
	var total int64
	for i, idx := range order {
		size := definitionBytes(cands[idx].def)
		if total+size > budget.MaxBytes && i > 0 {
			// Skip the entry that does not fit instead of stopping:
			// one oversized definition must not starve every
			// smaller, lower-priority candidate behind it. The
			// highest-priority entry is always kept, so a single
			// oversized tool stays visible rather than dead-ending.
			continue
		}
		total += size
		order[kept] = idx
		kept++
	}
	order = order[:kept]

	slices.SortFunc(order, func(a, b int32) int {
		return strings.Compare(cands[a].name, cands[b].name)
	})
	return order
}

// visibleDefinitions maps the visible order onto the definitions to send.
func visibleDefinitions(cands []candidate, st stateSnapshot, policy Policy) []message.ToolDefinition {
	order := visibleOrder(cands, st, policy)
	out := make([]message.ToolDefinition, 0, len(order))
	for _, idx := range order {
		out = append(out, cands[idx].def)
	}
	return out
}

// visibleCandidates maps the visible order onto the candidates. Callers that
// only need definitions or names go through visibleOrder and skip the copy.
func visibleCandidates(cands []candidate, st stateSnapshot, policy Policy) []candidate {
	order := visibleOrder(cands, st, policy)
	out := make([]candidate, 0, len(order))
	for _, idx := range order {
		out = append(out, cands[idx])
	}
	return out
}

func include(c candidate, st stateSnapshot, policy Policy) bool {
	required := st.isRequired(c.name)
	discovered := st.isDiscovered(c.name)
	switch c.exp {
	case ExposureAlways:
		return true
	case ExposureDirect:
		return required || discovered
	case ExposureDeferred:
		return required || discovered
	case ExposureHidden:
		return required
	default:
		return false
	}
}

func lessPriority(a, b candidate, st stateSnapshot) bool {
	if ar, br := a.exp.rank(), b.exp.rank(); ar != br {
		return ar < br
	}
	if ar, br := st.isRequired(a.name), st.isRequired(b.name); ar != br {
		return ar
	}
	ad, aOK := st.discovered[a.name]
	bd, bOK := st.discovered[b.name]
	if aOK != bOK {
		return aOK
	}
	if aOK && bOK {
		if ad.lastUse != bd.lastUse {
			return ad.lastUse > bd.lastUse
		}
		// Same round: a tool_search batch is ranked, so its better
		// hits win the visible cut. Separate discoveries of the same
		// round carry rank 0 and fall back to MRU order.
		if ad.rank != bd.rank {
			return ad.rank < bd.rank
		}
		if ad.seq != bd.seq {
			return ad.seq > bd.seq
		}
	}
	// tool_search is the discovery escape hatch: among otherwise equal
	// candidates it always sorts first, so per-round pruning keeps it.
	if a.name != b.name && (a.name == ToolName || b.name == ToolName) {
		return a.name == ToolName
	}
	return a.name < b.name
}
