package graph

import (
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// CanResume implements [agent.Resumer]: it validates that cp is a
// checkpoint this graph can meaningfully resume from, *before* the
// host spins up an execution.
//
// Graph checkpoints carry the most recently completed wave in
// [agent.Checkpoint.Steps] and the wave counter in Iteration — both
// produced by Execute's per-wave stamping. The ExecID-vs-run-id check
// happens in Execute, where the run id is available.
func (g *Graph) CanResume(cp agent.Checkpoint) error {
	if cp.SpecVersion != "" && cp.SpecVersion != g.specVersion {
		return errdefs.NotAvailablef(
			"graph %q: checkpoint spec version %q does not match current graph spec %q",
			g.name, cp.SpecVersion, g.specVersion)
	}
	if len(cp.Steps) == 0 {
		return errdefs.Validationf("graph %q: checkpoint has no position marker", g.name)
	}
	for _, id := range cp.Steps {
		if _, ok := g.nodes[id]; !ok {
			return errdefs.Validationf(
				"graph %q: checkpoint node %q not found (definition changed since checkpoint?)",
				g.name, id)
		}
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

// SuggestCheckpoint implements [agent.CheckpointSuggester]. Graph
// execution already stamps a checkpoint at every wave boundary, so a
// voluntary suggestion has nothing extra to do: the next completed
// wave persists one unconditionally. Returning nil documents that the
// suggestion is always honoured by construction.
func (g *Graph) SuggestCheckpoint() error { return nil }

// Compile-time assertion that the graph engine exposes the optional
// advisory checkpoint suggestion surface.
var _ agent.CheckpointSuggester = (*Graph)(nil)
