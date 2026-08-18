package graph

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Warning is a non-fatal build-time finding about the graph's
// topology or dataflow. Warnings never fail [Build]; they surface on
// [Graph.Warnings] for tooling, linters and tests.
type Warning struct {
	Kind    WarningKind
	NodeID  string
	Message string
}

// WarningKind classifies build warnings.
type WarningKind string

const (
	// WarningUnreachableNode marks nodes no path from the entry can
	// reach. Conditions are ignored during this check, so a node may
	// be flagged even though a statically-false condition is the real
	// cause — the warning is advisory.
	WarningUnreachableNode WarningKind = "unreachable_node"

	// WarningDeadEndNode marks nodes with no outgoing edges. A run
	// reaching them simply ends. Usually intentional for terminal
	// nodes, worth double-checking for the rest.
	WarningDeadEndNode WarningKind = "dead_end_node"

	// WarningUnguardedCycle marks a strongly-connected component in
	// which every edge is unconditional: a run entering it can only
	// leave via the MaxIterations budget guard. Cycles with at least
	// one conditional edge are considered guarded — the condition is
	// the exit.
	WarningUnguardedCycle WarningKind = "unguarded_cycle"

	// WarningMissingDefaultBranch marks nodes with several outgoing
	// edges, some conditional and none unconditional: when no
	// condition fires the branch stops silently, which is rarely the
	// intent behind a multi-way split.
	WarningMissingDefaultBranch WarningKind = "missing_default_branch"

	// WarningUnresolvedReference marks "${board.<name>}" config
	// references whose variable no node's declared writes produce and
	// the kernel does not provide. Advisory: the host may still seed
	// the variable before the run (e.g. user input) — in that case
	// the warning can be ignored.
	WarningUnresolvedReference WarningKind = "unresolved_reference"

	// WarningParallelNoJoin marks unconditional fan-outs whose
	// branches share no common successor. Advisory: multi-terminal
	// graphs are legal, but an unintended missing join silently
	// drops the "rest" of the pipeline.
	WarningParallelNoJoin WarningKind = "parallel_no_join"
)

// analyzeGraph derives build-time warnings from the assembled graph.
// Node slots carry the statically resolved I/O roles, which is what
// makes the dataflow check (WarningUnresolvedReference) honest rather
// than heuristic.
func analyzeGraph(entry string, nodes map[string]*nodeSlot, order []string, edges map[string][]Edge) []Warning {
	var warnings []Warning
	warnings = append(warnings, checkTopology(entry, nodes, order, edges)...)
	warnings = append(warnings, checkUnguardedCycles(nodes, edges)...)
	warnings = append(warnings, checkMissingDefaultBranch(order, edges)...)
	warnings = append(warnings, checkUnresolvedReferences(nodes, order)...)
	warnings = append(warnings, checkParallelNoJoin(order, edges)...)
	return warnings
}

// checkTopology reports unreachable and dead-end nodes.
func checkTopology(entry string, nodes map[string]*nodeSlot, order []string, edges map[string][]Edge) []Warning {
	var warnings []Warning

	reachable := map[string]bool{entry: true}
	queue := []string{entry}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, e := range edges[id] {
			if e.To == END || reachable[e.To] {
				continue
			}
			reachable[e.To] = true
			queue = append(queue, e.To)
		}
	}

	for _, id := range order {
		if !reachable[id] {
			warnings = append(warnings, Warning{
				Kind:    WarningUnreachableNode,
				NodeID:  id,
				Message: "node is not reachable from the entry node",
			})
		}
		if len(edges[id]) == 0 {
			warnings = append(warnings, Warning{
				Kind:    WarningDeadEndNode,
				NodeID:  id,
				Message: "node has no outgoing edges; a run reaching it ends here",
			})
		}
	}
	return warnings
}

// checkUnguardedCycles reports strongly-connected components whose
// internal edges are all unconditional.
func checkUnguardedCycles(nodes map[string]*nodeSlot, edges map[string][]Edge) []Warning {
	var warnings []Warning
	for _, scc := range stronglyConnectedComponents(nodes, edges) {
		unguarded := true
		for _, from := range scc {
			for _, e := range edges[from] {
				if e.Condition == nil && contains(scc, e.To) {
					continue // unconditional edge staying inside the cycle
				}
				// A conditional edge, or any edge leaving the cycle
				// (including END), is state-dependent or direct
				// routing out of it — the cycle has an exit.
				unguarded = false
			}
		}
		if unguarded {
			warnings = append(warnings, Warning{
				Kind:    WarningUnguardedCycle,
				NodeID:  scc[0],
				Message: fmt.Sprintf("cycle %v has no conditional exit edge; only the MaxIterations guard can stop it", scc),
			})
		}
	}
	return warnings
}

// checkMissingDefaultBranch reports multi-way splits with conditions
// on some edges but no unconditional fallback.
func checkMissingDefaultBranch(order []string, edges map[string][]Edge) []Warning {
	var warnings []Warning
	for _, id := range order {
		outs := edges[id]
		if len(outs) < 2 {
			continue
		}
		conditional, unconditional := 0, 0
		for _, e := range outs {
			if e.Condition == nil {
				unconditional++
			} else {
				conditional++
			}
		}
		if conditional > 0 && unconditional == 0 {
			warnings = append(warnings, Warning{
				Kind:    WarningMissingDefaultBranch,
				NodeID:  id,
				Message: "node has conditional edges but no default (unconditional) branch; the run stops here when nothing fires",
			})
		}
	}
	return warnings
}

// checkUnresolvedReferences reports config board references no
// declared write produces. The set of provided variables comes from
// statically resolved write roles plus the kernel's own reserved vars.
func checkUnresolvedReferences(nodes map[string]*nodeSlot, order []string) []Warning {
	provided := map[string]bool{
		VarInterruptedNode: true,
		VarToolCalls:       true,
	}
	for _, id := range order {
		for _, w := range nodes[id].writes {
			if w.Kind == RoleVar {
				provided[w.Key] = true
			}
		}
	}

	var warnings []Warning
	for _, id := range order {
		slot := nodes[id]
		if len(slot.def.Config) == 0 {
			continue
		}
		var cfg any
		if err := json.Unmarshal(slot.def.Config, &cfg); err != nil {
			continue // malformed config already fails Build elsewhere
		}
		for _, ref := range ExtractRefs(cfg) {
			// A nested path counts as provided when any node writes its
			// root segment (e.g. ${board.user.name} is satisfied by a
			// write to "user").
			root := ref
			if i := strings.IndexByte(ref, '.'); i >= 0 {
				root = ref[:i]
			}
			if !provided[ref] && !provided[root] {
				warnings = append(warnings, Warning{
					Kind:   WarningUnresolvedReference,
					NodeID: id,
					Message: fmt.Sprintf(
						"config references ${board.%s} but no node declares a write for it (host-seeded input is fine — ignore if intentional)",
						ref),
				})
			}
		}
	}
	return warnings
}

// checkParallelNoJoin reports unconditional fan-outs whose branches
// never rejoin at a common successor.
func checkParallelNoJoin(order []string, edges map[string][]Edge) []Warning {
	var warnings []Warning
	for _, id := range order {
		var targets []string
		for _, e := range edges[id] {
			if e.Condition == nil && e.To != END {
				targets = append(targets, e.To)
			}
		}
		if len(targets) < 2 {
			continue
		}
		if commonSuccessor(edges, targets) == "" {
			warnings = append(warnings, Warning{
				Kind:   WarningParallelNoJoin,
				NodeID: id,
				Message: fmt.Sprintf(
					"node forks unconditionally into %v with no common join node; branches end independently",
					targets),
			})
		}
	}
	return warnings
}

// commonSuccessor returns a node reachable from every target, or "".
func commonSuccessor(edges map[string][]Edge, targets []string) string {
	reachableFrom := func(start string) map[string]bool {
		seen := map[string]bool{}
		queue := []string{start}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			for _, e := range edges[id] {
				if e.To == END || seen[e.To] {
					continue
				}
				seen[e.To] = true
				queue = append(queue, e.To)
			}
		}
		return seen
	}

	common := reachableFrom(targets[0])
	for _, t := range targets[1:] {
		next := reachableFrom(t)
		for id := range common {
			if !next[id] {
				delete(common, id)
			}
		}
	}
	// A node reachable from itself via a cycle does not count as its
	// own join.
	for _, t := range targets {
		if common[t] && !reachableFrom(t)[t] {
			delete(common, t)
		}
	}
	if len(common) == 0 {
		return ""
	}
	sorted := make([]string, 0, len(common))
	for id := range common {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	return sorted[0]
}

// stronglyConnectedComponents returns the cyclic components of the
// graph (Tarjan's algorithm): each component is either several nodes
// or a single node with a self-loop. Conditions are ignored.
func stronglyConnectedComponents(nodes map[string]*nodeSlot, edges map[string][]Edge) [][]string {
	index := map[string]int{}
	lowlink := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var sccs [][]string
	counter := 0

	var visit func(id string)
	visit = func(id string) {
		index[id] = counter
		lowlink[id] = counter
		counter++
		stack = append(stack, id)
		onStack[id] = true

		for _, e := range edges[id] {
			if e.To == END {
				continue
			}
			if _, seen := index[e.To]; !seen {
				visit(e.To)
				if lowlink[e.To] < lowlink[id] {
					lowlink[id] = lowlink[e.To]
				}
			} else if onStack[e.To] && index[e.To] < lowlink[id] {
				lowlink[id] = index[e.To]
			}
		}

		if lowlink[id] == index[id] {
			var scc []string
			for {
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[top] = false
				scc = append(scc, top)
				if top == id {
					break
				}
			}
			selfLoop := false
			for _, e := range edges[id] {
				if e.To == id {
					selfLoop = true
				}
			}
			if len(scc) > 1 || selfLoop {
				sort.Strings(scc)
				sccs = append(sccs, scc)
			}
		}
	}

	for id := range nodes {
		if _, seen := index[id]; !seen {
			visit(id)
		}
	}
	return sccs
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
