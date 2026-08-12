// Package agent is the single home of the agent model:
//
//   - [Definition] — the serializable document form used by
//     deployment documents: card, tool allow-list, engine selection
//     ([EngineRef]), resource bindings, and lifecycle hooks by slot
//     (prepare / observe / referees / commit).
//   - [Agent] — the assembled runtime form built from a Definition by
//     the deployment layer: identity, card, tools, the constructed
//     engine ([Engine]), and the attached hooks
//     ([Preparer] / [Observer] / [Referee] / [Committer]). core/deploy
//     returns this type instead of defining its own bound-agent shape.
//   - [Board] / [BoardSnapshot] — the engine execution blackboard and
//     its serialisable snapshot, shared by engines and checkpointing.
//   - [Checkpoint] / [CheckpointStore] — the resume record envelope
//     and the host-side persistence contract. Concrete stores are
//     resource implementations under checkpoint/ (e.g.
//     checkpoint/workspace, registered as checkpoint.Store/workspace).
//   - Runtime — [Execute] (the turn harness), [Run] / [Request] /
//     [Result] / [Identity], [Host] (host-side capabilities),
//     [Board], and the lifecycle contracts.
package agent
