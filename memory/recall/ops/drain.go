package ops

import (
	"context"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
)

// DrainResult summarizes one pass across a target.
type DrainResult struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Scopes     []ScopeDrainResult
}

// ScopeDrainResult summarizes one pass for one scope.
type ScopeDrainResult struct {
	Scope         recall.Scope
	SideEffects   recall.SideEffectProcessResult
	AsyncSemantic recall.AsyncSemanticProcessResult
	Duration      time.Duration
	Err           string
}

// TotalClaimed returns side-effect + async semantic jobs claimed by this pass.
func (r DrainResult) TotalClaimed() int {
	n := 0
	for _, scope := range r.Scopes {
		n += scope.SideEffects.Claimed + scope.AsyncSemantic.Claimed
	}
	return n
}

// Drain resolves target and processes one batch per scope.
func (r *Runner) Drain(ctx context.Context, target Target) (DrainResult, error) {
	scopes, err := r.resolveScopes(ctx, target)
	if err != nil {
		return DrainResult{}, err
	}
	return r.DrainScopes(ctx, scopes)
}

// DrainRuntime processes one batch per enumerated runtime scope.
func (r *Runner) DrainRuntime(ctx context.Context, runtimeID string) (DrainResult, error) {
	return r.Drain(ctx, Target{RuntimeID: runtimeID})
}

// DrainScopes processes one batch for each supplied scope.
func (r *Runner) DrainScopes(ctx context.Context, scopes []recall.Scope) (DrainResult, error) {
	scopes, err := normalizeScopes(scopes)
	if err != nil {
		return DrainResult{}, err
	}
	started := r.cfg.Now()
	results := r.forEachScope(ctx, scopes, r.drainScope)
	res := DrainResult{
		StartedAt:  started,
		FinishedAt: r.cfg.Now(),
		Scopes:     results,
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	failures := failuresFromDrain(results)
	if len(failures) > 0 {
		return res, Error{Failures: failures}
	}
	return res, nil
}

func (r *Runner) drainScope(ctx context.Context, scope recall.Scope) ScopeDrainResult {
	started := r.cfg.Now()
	res := ScopeDrainResult{Scope: scope}
	var failures []Failure
	now := r.cfg.Now()
	if r.cfg.DrainSideEffects && r.side != nil {
		side, err := r.side.ProcessSideEffects(ctx, recall.SideEffectProcessOptions{
			WorkerID: r.cfg.WorkerID,
			Scope:    scope,
			Limit:    r.cfg.BatchSize,
			Now:      now,
		})
		res.SideEffects = side
		if err != nil {
			failures = append(failures, Failure{Scope: scope, Operation: "side_effects", Err: err})
		}
	}
	if r.cfg.DrainAsyncSemantic && r.async != nil {
		async, err := r.async.ProcessAsyncSemantic(ctx, recall.AsyncSemanticProcessOptions{
			WorkerID: r.cfg.WorkerID,
			Scope:    scope,
			Limit:    r.cfg.BatchSize,
			Now:      now,
		})
		res.AsyncSemantic = async
		if err != nil {
			failures = append(failures, Failure{Scope: scope, Operation: "async_semantic", Err: err})
		}
	}
	res.Duration = r.cfg.Now().Sub(started)
	if len(failures) > 0 {
		res.Err = (Error{Failures: failures}).Error()
	}
	event := Event{
		Time:     r.cfg.Now(),
		Kind:     EventDrain,
		Scope:    scope,
		Duration: res.Duration,
		Err:      res.Err,
		Drain:    &res,
	}
	r.emit(ctx, event)
	return res
}

func failuresFromDrain(results []ScopeDrainResult) []Failure {
	var failures []Failure
	for _, res := range results {
		if res.Err == "" {
			continue
		}
		failures = append(failures, Failure{
			Scope:     res.Scope,
			Operation: "drain",
			Err:       errors.New(res.Err),
		})
	}
	return failures
}
