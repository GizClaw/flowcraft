package runner

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/graph/runner/internal/executor"
)

// This file re-exports the small subset of the (internal) executor
// package that legitimate callers need to interact with: parallel
// execution policy types for runner.WithParallel, the pluggable
// variable resolver contract, the pluggable merge function contract,
// and the actor-key context helper. Anything not re-exported here is
// an implementation detail of the engine that lives entirely behind
// runner.Runner.
//
// Subject helpers are NOT re-exported. Subscribers should import
// sdk/engine and use engine.PatternRun / engine.PatternAllRuns /
// engine.PatternRunSteps / engine.PatternRunStream — the runner
// publishes envelopes under the engine-contract subject convention,
// so engine's helpers are the canonical entry point.
//
// Each alias names a single concept the executor owns; the runner
// package never grows its own definition for these so there is one
// canonical type the user can satisfy / pass around.

// --- parallel execution ------------------------------------------------------

// ParallelConfig configures parallel fork/join execution. Passed to
// [WithParallel].
type ParallelConfig = executor.ParallelConfig

// MergeStrategy names a parallel-branch merge policy.
type MergeStrategy = executor.MergeStrategy

// Built-in merge strategies. RegisterMergeStrategy lets callers add
// their own.
const (
	MergeLastWins        = executor.MergeLastWins
	MergeNamespace       = executor.MergeNamespace
	MergeErrorOnConflict = executor.MergeErrorOnConflict
)

// MergeFunc is the signature of a parallel merge implementation.
type MergeFunc = executor.MergeFunc

// RegisterMergeStrategy registers fn under name so [ParallelConfig]
// can refer to it by string. Callers SHOULD register custom strategies
// at init() time.
func RegisterMergeStrategy(name MergeStrategy, fn MergeFunc) {
	executor.RegisterMergeStrategy(name, fn)
}

// --- variable resolver contract ---------------------------------------------

// VariableResolver resolves variable references in node configs. The
// runner installs a default variable.NewResolver() per execution; supply
// your own via [WithResolver] when you need a different scope or
// resolution policy.
type VariableResolver = executor.VariableResolver

// CloneableResolver is the optional interface a resolver implements to
// support parallel branches. Branches need independent scope so the
// runner clones the resolver for each.
type CloneableResolver = executor.CloneableResolver

// --- context helpers ---------------------------------------------------------

// WithActorKey stamps an agent identifier onto ctx so the runner
// forwards it onto every envelope header (HeaderAgentID) and uses
// it as the prefix of the step subject segment.
//
// Deprecated: as of v0.4 the runner resolves the agent id from the
// canonical [engine.Run.Attributes][telemetry.AttrAgentID] key first
// (populated automatically by [agent.Run]) and only falls back to
// this ctx-key when the attribute is absent. Prefer driving the
// runner through [agent.Run] (or stamp the attribute directly on
// engine.Run.Attributes) — that path survives cross-process
// hand-offs (HTTP, vessel inline, A2A) where context values are
// dropped at the wire boundary. WithActorKey will be removed in
// v0.5.0.
func WithActorKey(ctx context.Context, key string) context.Context {
	return executor.WithActorKey(ctx, key)
}
