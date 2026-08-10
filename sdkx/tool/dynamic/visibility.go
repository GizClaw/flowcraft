package dynamic

import (
	"sort"

	"github.com/GizClaw/flowcraft/sdk/message"
)

// candidate couples one registry tool with its exposure and definition
// for the visibility computation.
type candidate struct {
	name string
	def  message.Definition
	exp  Exposure
}

// visibleCandidates applies the visibility algorithm and returns the
// definitions that reach the model this round, sorted by name for
// stable output. The computation is pure: it never mutates the catalog
// or the state.
func visibleCandidates(cands []candidate, st stateSnapshot, policy Policy) []candidate {
	budget := policy.budget()
	visible := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if include(c, st, policy) {
			visible = append(visible, c)
		}
	}

	// Deterministic pruning order: exposure rank, RequiredByName,
	// selected rounds, recency, then name.
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
	selected := st.selected[c.name] > 0
	recent := st.isRecent(c.name, policy.recentWindow())
	switch c.exp {
	case ExposureAlways:
		return true
	case ExposureDirect:
		return required || selected || recent
	case ExposureDeferred:
		return required || selected
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
	if as, bs := st.selected[a.name], st.selected[b.name]; as != bs {
		return as > bs
	}
	if ar, br := st.recent[a.name], st.recent[b.name]; ar != br {
		return ar > br
	}
	return a.name < b.name
}
