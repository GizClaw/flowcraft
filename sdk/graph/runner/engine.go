package runner

import (
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// engine.go isolates the engine.Engine / engine.Describer / engine.Resumer
// glue for [Runner]. The Execute implementation lives in runner.go
// alongside the other execution paths so the option-assembly logic
// stays in one place; this file only carries the policies that diverge
// from a plain graph dispatch — capability declarations and the
// resume admission probes.

// Compile-time checks: Runner satisfies engine.Engine + engine.Describer
// + engine.Resumer. If any of these break the agent runtime would
// silently regress (refuse to drive Runner / treat it as zero
// capabilities / refuse to resume) — keep the assertions here so a
// signature drift fails the build instead of becoming a runtime
// surprise.
var (
	_ engine.Engine    = (*Runner)(nil)
	_ engine.Describer = (*Runner)(nil)
	_ engine.Resumer   = (*Runner)(nil)
)

// Capabilities reports what this engine implementation can do, per the
// [engine.Describer] contract. Hosts (agent.Run preflight, vessel
// build path, dashboards) read this to gate features on.
//
// Current values:
//
//   - SupportsResume = true. Execute consumes Run.ResumeFrom: it
//     restores board state from cp.Board and continues from cp.Step's
//     downstream edges. CanResume below is the synchronous probe.
//   - EmitsUserPrompt = false. The runner core never calls
//     host.AskUser. Optional plugin nodes (e.g. scriptnode) MAY
//     prompt the user via the host bridge; that is the user's graph
//     decision, not a runner-intrinsic capability, so it is left
//     unclaimed at the engine level.
//   - EmitsCheckpoint = true. The internal executor calls
//     host.Checkpoint after every node completes. Pods that need
//     durable replay should attach a CheckpointStore.
//   - RequiredDepNames = nil. The graph runner is a meta-engine —
//     concrete dep needs (LLM clients, tool registries) are
//     declared per-node-factory by whoever assembled the graph,
//     not by the runner itself.
func (r *Runner) Capabilities() engine.Capabilities {
	return engine.Capabilities{
		SupportsResume:   true,
		EmitsUserPrompt:  false,
		EmitsCheckpoint:  true,
		RequiredDepNames: nil,
	}
}

// CanResume satisfies [engine.Resumer]. It is the cheap pre-flight
// probe a host runs before invoking Execute with a non-nil ResumeFrom
// so obvious incompatibilities surface as typed errors instead of
// failing partway through Execute.
//
// Checks performed (all errdefs.Validation when violated — these are
// programmer errors, not transient conditions):
//
//   - cp.Step is non-empty: a checkpoint without a step marker has no
//     "where to resume" and cannot be replayed.
//   - cp.Step names a node present in this Runner's compiled graph:
//     prevents reusing a checkpoint produced by a different graph
//     definition (rename / restructure ⇒ resume becomes invalid).
//   - cp.Attributes["graph_name"] (if set) matches this Runner's
//     compiled graph name: defends against the cross-graph case where
//     two graphs happen to share node ids.
//
// CanResume does NOT perform the foreign-ExecID check — that lives in
// validateResume on the Execute path because it requires the calling
// Run.ID, which CanResume does not receive.
func (r *Runner) CanResume(cp engine.Checkpoint) error {
	if cp.Step == "" {
		return errdefs.Validationf("graph runner: resume: checkpoint has no Step marker")
	}
	if r.compiled == nil || r.compiled.Graph == nil {
		return errdefs.Validationf("graph runner: resume: runner has no compiled graph")
	}
	graphName := r.compiled.Graph.Name
	if cpGraph := cp.Attributes["graph_name"]; cpGraph != "" && cpGraph != graphName {
		return errdefs.Validationf(
			"graph runner: resume: checkpoint graph_name %q does not match runner graph %q",
			cpGraph, graphName)
	}
	// CanResume runs on the host's resume path — keep it cheap by
	// scanning the compiled NodeDefs list rather than re-assembling
	// the graph for an O(1) lookup. Graph definitions are typically
	// small (<100 nodes); the linear scan is negligible.
	found := false
	for _, nd := range r.compiled.NodeDefs {
		if nd.ID == cp.Step {
			found = true
			break
		}
	}
	if !found {
		return errdefs.Validationf(
			"graph runner: resume: checkpoint Step %q is not a node in graph %q",
			cp.Step, graphName)
	}
	return nil
}

// validateResume runs the per-Execute admission checks the engine.Engine
// contract requires before the executor sees Run.ResumeFrom:
//
//   - Foreign ExecID → errdefs.Validation. Supplying a checkpoint that
//     belongs to a different run is "trying to fork", not "trying to
//     resume"; cross-pollinating state is a programmer error.
//   - All [Resumer.CanResume] checks (graph match, step exists, …) so
//     a host that calls Execute directly without invoking CanResume
//     itself still gets the same protections.
func (r *Runner) validateResume(cp engine.Checkpoint, runID string) error {
	if cp.ExecID != "" && runID != "" && cp.ExecID != runID {
		return errdefs.Validationf(
			"graph runner: ResumeFrom.ExecID %q does not match Run.ID %q "+
				"(use a fresh Run.ID to fork instead of resume)",
			cp.ExecID, runID)
	}
	return r.CanResume(cp)
}
