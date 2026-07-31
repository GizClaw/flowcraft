package graph

import (
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// CanResume implements [agent.Resumer]: it validates that cp is a
// checkpoint this graph can meaningfully resume from, *before* the
// host spins up an execution.
//
// Graph checkpoints carry the last executed node id in
// [agent.Checkpoint.Step] and the wave counter in Iteration — both
// produced by Execute's per-wave stamping. The ExecID-vs-run-id check
// happens in Execute, where the run id is available.
func (g *Graph) CanResume(cp agent.Checkpoint) error {
	if cp.Step == "" {
		return errdefs.Validationf("graph %q: checkpoint has no node marker", g.name)
	}
	if _, ok := g.nodes[cp.Step]; !ok {
		return errdefs.Validationf(
			"graph %q: checkpoint node %q not found (definition changed since checkpoint?)",
			g.name, cp.Step)
	}
	if cp.Board == nil {
		return errdefs.Validationf("graph %q: checkpoint carries no board state", g.name)
	}
	if g.maxIterations > 0 && cp.Iteration > g.maxIterations {
		return errdefs.Validationf(
			"graph %q: checkpoint iteration %d exceeds max iterations %d",
			g.name, cp.Iteration, g.maxIterations)
	}
	return nil
}
