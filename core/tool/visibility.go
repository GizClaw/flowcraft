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
	// discovery-pool recency, then name.
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
			break
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
