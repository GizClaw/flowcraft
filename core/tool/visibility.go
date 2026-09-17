package tool

import (
	"sort"

	"github.com/GizClaw/flowcraft/core/message"
)

// candidate couples one registry tool with its exposure and definition
// for the visibility computation.
type candidate struct {
	name string
	def  message.ToolDefinition
	exp  Exposure
}

// visibleCandidates applies the visibility algorithm and returns the
// definitions that reach the model this round, sorted by name for
// stable output. The computation is pure: it never mutates the session
// or the policy.
func visibleCandidates(cands []candidate, st stateSnapshot, policy Policy) []candidate {
	budget := policy.Budget
	visible := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if include(c, st, policy) {
			visible = append(visible, c)
		}
	}

	// Deterministic pruning order: exposure rank, RequiredByName,
	// discovery-pool recency, then name. Same-round pool entries share
	// a lastUse, so they fall back to discovery order — the first
	// (best-ranked) hit of a tool_search batch wins the cut.
	sort.SliceStable(visible, func(i, j int) bool {
		return lessPriority(visible[i], visible[j], st)
	})
	if len(visible) > budget.MaxDefinitions {
		visible = visible[:budget.MaxDefinitions]
	}

	kept := make([]candidate, 0, len(visible))
	var total int64
	for i, c := range visible {
		size := definitionBytes(c.def)
		if total+size > budget.MaxBytes && i > 0 {
			// Skip the entry that does not fit instead of stopping:
			// one oversized definition must not starve every
			// smaller, lower-priority candidate behind it. The
			// highest-priority entry is always kept, so a single
			// oversized tool stays visible rather than dead-ending.
			continue
		}
		total += size
		kept = append(kept, c)
	}

	sort.Slice(kept, func(i, j int) bool {
		return kept[i].name < kept[j].name
	})
	return kept
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
