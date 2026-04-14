package compiler

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
)

// analyze performs static analysis on the compiled graph, returning warnings.
func analyze(g *graph.RawGraph, def *graph.GraphDefinition) []Warning {
	var warnings []Warning
	warnings = append(warnings, checkDeadEnds(g)...)
	warnings = append(warnings, checkUnguardedCycles(g, def)...)
	warnings = append(warnings, checkConditionCoverage(g)...)
	warnings = append(warnings, checkVariableReferences(def)...)
	warnings = append(warnings, checkSkipConditions(def)...)
	warnings = append(warnings, CheckPortCompatibility(g)...)
	warnings = append(warnings, checkParallelJoin(g)...)
	warnings = append(warnings, checkLLMMessagesKey(def)...)
	return warnings
}

// checkDeadEnds detects nodes that cannot reach __end__.
func checkDeadEnds(g *graph.RawGraph) []Warning {
	reachable := bfsReachableReverse(g)
	var dead []string
	for id := range g.Nodes {
		if !reachable[id] && id != graph.END {
			dead = append(dead, id)
		}
	}
	if len(dead) == 0 {
		return nil
	}
	return []Warning{{
		Code:    "unreachable_end",
		Message: "nodes cannot reach __end__",
		NodeIDs: dead,
	}}
}

// bfsReachableReverse finds all nodes that can reach __end__ via reverse edges.
func bfsReachableReverse(g *graph.RawGraph) map[string]bool {
	reachable := make(map[string]bool)
	reachable[graph.END] = true

	queue := []string{graph.END}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, from := range g.Reverse[cur] {
			if !reachable[from] {
				reachable[from] = true
				queue = append(queue, from)
			}
		}
	}
	return reachable
}

// checkUnguardedCycles warns about cycles that lack a LoopGuard node.
func checkUnguardedCycles(g *graph.RawGraph, def *graph.GraphDefinition) []Warning {
	if !detectCycles(g) {
		return nil
	}

	hasLoopGuard := make(map[string]bool)
	for _, nd := range def.Nodes {
		if nd.Type == "loopguard" {
			hasLoopGuard[nd.ID] = true
		}
	}

	cycles := findCycleNodes(g)
	var unguarded []string
	for _, nodeID := range cycles {
		guardFound := false
		for _, e := range g.Edges[nodeID] {
			if hasLoopGuard[e.To] {
				guardFound = true
				break
			}
		}
		if hasLoopGuard[nodeID] {
			guardFound = true
		}
		if !guardFound {
			unguarded = append(unguarded, nodeID)
		}
	}

	if len(unguarded) == 0 {
		return nil
	}
	return []Warning{{
		Code:    "unguarded_cycle",
		Message: "cycle detected without LoopGuard exit",
		NodeIDs: unguarded,
	}}
}

// findCycleNodes returns all node IDs that participate in at least one cycle.
func findCycleNodes(g *graph.RawGraph) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	colors := make(map[string]int, len(g.Nodes))
	for id := range g.Nodes {
		colors[id] = white
	}

	cycleNodes := make(map[string]bool)
	var stack []string

	var dfs func(string) bool
	dfs = func(id string) bool {
		colors[id] = gray
		stack = append(stack, id)

		for _, e := range g.Edges[id] {
			if e.To == graph.END {
				continue
			}
			switch colors[e.To] {
			case gray:
				inCycle := false
				for _, s := range stack {
					if s == e.To {
						inCycle = true
					}
					if inCycle {
						cycleNodes[s] = true
					}
				}
				return true
			case white:
				dfs(e.To)
			}
		}

		stack = stack[:len(stack)-1]
		colors[id] = black
		return false
	}

	for id := range g.Nodes {
		if colors[id] == white {
			dfs(id)
		}
	}

	result := make([]string, 0, len(cycleNodes))
	for id := range cycleNodes {
		result = append(result, id)
	}
	return result
}

// checkConditionCoverage warns about nodes with conditional edges but no default branch.
func checkConditionCoverage(g *graph.RawGraph) []Warning {
	var warnings []Warning
	for nodeID, edges := range g.Edges {
		if len(edges) <= 1 {
			continue
		}

		hasConditional := false
		hasUnconditional := false
		for _, e := range edges {
			if e.Condition != nil {
				hasConditional = true
			} else {
				hasUnconditional = true
			}
		}

		if hasConditional && !hasUnconditional {
			warnings = append(warnings, Warning{
				Code:    "missing_default_branch",
				Message: "node has conditional edges but no default (unconditional) branch",
				NodeIDs: []string{nodeID},
			})
		}
	}
	return warnings
}

// checkVariableReferences extracts all ${scope.name} variable references from
// node configs and warns about any whose scope is not one of the well-known
// scopes (input, output, env, system, board).
func checkVariableReferences(def *graph.GraphDefinition) []Warning {
	wellKnown := map[string]bool{
		"input": true, "output": true, "env": true,
		"system": true, "board": true, "node": true,
	}
	var warnings []Warning
	for _, nd := range def.Nodes {
		for _, v := range nd.Config {
			s, ok := v.(string)
			if !ok {
				continue
			}
			refs := variable.ExtractRefs(s)
			for _, ref := range refs {
				parts := strings.SplitN(ref, ".", 2)
				if len(parts) < 2 {
					warnings = append(warnings, Warning{
						Code:    "invalid_variable_ref",
						Message: fmt.Sprintf("node %q has malformed variable reference ${%s}", nd.ID, ref),
						NodeIDs: []string{nd.ID},
					})
					continue
				}
				if !wellKnown[parts[0]] {
					warnings = append(warnings, Warning{
						Code:    "unknown_variable_scope",
						Message: fmt.Sprintf("node %q references unknown scope %q in ${%s}", nd.ID, parts[0], ref),
						NodeIDs: []string{nd.ID},
					})
				}
			}
		}
	}
	return warnings
}

// checkSkipConditions validates that all SkipCondition expressions on nodes
// are syntactically valid expr-lang expressions before runtime.
func checkSkipConditions(def *graph.GraphDefinition) []Warning {
	var warnings []Warning
	for _, nd := range def.Nodes {
		if nd.SkipCondition == "" {
			continue
		}
		if _, err := graph.CompileCondition(nd.SkipCondition); err != nil {
			warnings = append(warnings, Warning{
				Code:    "invalid_skip_condition",
				Message: fmt.Sprintf("node %q has invalid skip_condition: %v", nd.ID, err),
				NodeIDs: []string{nd.ID},
			})
		}
	}
	return warnings
}

// checkParallelJoin warns when a node has multiple unconditional outgoing edges
// (parallel fork) but the branches have no common join node.
func checkParallelJoin(g *graph.RawGraph) []Warning {
	succs := successors(g)
	var warnings []Warning

	for nodeID, edges := range g.Edges {
		var uncondTargets []string
		for _, e := range edges {
			if e.Condition == nil {
				uncondTargets = append(uncondTargets, e.To)
			}
		}
		if len(uncondTargets) < 2 {
			continue
		}

		if findCommonSuccessor(succs, uncondTargets) == "" {
			warnings = append(warnings, Warning{
				Code:    "parallel_no_join",
				Message: fmt.Sprintf("node %q forks into parallel branches with no common join node", nodeID),
				NodeIDs: append([]string{nodeID}, uncondTargets...),
			})
		}
	}
	return warnings
}

// findCommonSuccessor finds the first node reachable from all branch starts via BFS.
func findCommonSuccessor(succs map[string][]string, branchStarts []string) string {
	if len(branchStarts) == 0 {
		return ""
	}

	startSet := make(map[string]bool, len(branchStarts))
	for _, id := range branchStarts {
		startSet[id] = true
	}

	reachSets := make([]map[string]bool, len(branchStarts))
	for i, sid := range branchStarts {
		reachSets[i] = bfsReachableForward(succs, sid)
	}

	visited := make(map[string]bool)
	queue := []string{branchStarts[0]}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true

		if !startSet[cur] {
			allReach := true
			for j := 1; j < len(reachSets); j++ {
				if !reachSets[j][cur] {
					allReach = false
					break
				}
			}
			if allReach {
				return cur
			}
		}

		for _, next := range succs[cur] {
			if next != graph.END && !visited[next] {
				queue = append(queue, next)
			}
		}
	}
	return ""
}

// bfsReachableForward finds all nodes reachable from start via forward edges.
func bfsReachableForward(succs map[string][]string, start string) map[string]bool {
	reached := make(map[string]bool)
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if reached[cur] || cur == graph.END {
			continue
		}
		reached[cur] = true
		queue = append(queue, succs[cur]...)
	}
	return reached
}

// checkLLMMessagesKey warns when an LLM node uses a non-default messages_key
// without enabling query_fallback, which causes the isolated message list to
// start empty and the LLM to receive no user input.
func checkLLMMessagesKey(def *graph.GraphDefinition) []Warning {
	var warnings []Warning
	for _, nd := range def.Nodes {
		if nd.Type != "llm" {
			continue
		}
		mk, _ := nd.Config["messages_key"].(string)
		if mk == "" || mk == "messages" {
			continue
		}
		qf, _ := nd.Config["query_fallback"].(bool)
		if qf {
			continue
		}
		warnings = append(warnings, Warning{
			Code: "llm_isolated_messages_no_fallback",
			Message: fmt.Sprintf(
				"node %q uses messages_key=%q but query_fallback is not enabled; "+
					"the isolated message list starts empty so the LLM receives no user input — "+
					"set query_fallback=true or use the default messages_key",
				nd.ID, mk),
			NodeIDs: []string{nd.ID},
		})
	}
	return warnings
}

// CheckPortCompatibility checks if connected nodes have compatible port types.
func CheckPortCompatibility(g *graph.RawGraph) []Warning {
	var warnings []Warning

	for _, edges := range g.Edges {
		for _, e := range edges {
			fromNode, fromOk := g.Nodes[e.From]
			toNode, toOk := g.Nodes[e.To]
			if !fromOk || !toOk {
				continue
			}

			fromPD, fromHasPorts := fromNode.(graph.PortDeclarable)
			toPD, toHasPorts := toNode.(graph.PortDeclarable)
			if !fromHasPorts || !toHasPorts {
				continue
			}

			outPorts := make(map[string]graph.PortType)
			for _, p := range fromPD.OutputPorts() {
				outPorts[p.Name] = p.Type
			}

			for _, ip := range toPD.InputPorts() {
				if ip.Required {
					opType, exists := outPorts[ip.Name]
					if !exists {
						continue
					}
					if !graph.IsCompatible(opType, ip.Type) {
						warnings = append(warnings, Warning{
							Code: "port_incompatible",
							Message: "output port " + ip.Name + " (" + string(opType) +
								") is not compatible with input port (" + string(ip.Type) + ")",
							NodeIDs: []string{e.From, e.To},
						})
					}
				}
			}
		}
	}
	return warnings
}
