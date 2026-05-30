package ops

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
)

// ReadinessResult summarizes readiness across one target.
type ReadinessResult struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Status     recall.ReadinessStatus
	Reports    []recall.ReadinessReport
}

// Readiness resolves target and checks every scope.
func (r *Runner) Readiness(ctx context.Context, target Target) (ReadinessResult, error) {
	scopes, err := r.resolveScopes(ctx, target)
	if err != nil {
		return ReadinessResult{}, err
	}
	return r.ReadinessScopes(ctx, scopes)
}

// ReadinessRuntime checks every enumerated scope in a runtime.
func (r *Runner) ReadinessRuntime(ctx context.Context, runtimeID string) (ReadinessResult, error) {
	return r.Readiness(ctx, Target{RuntimeID: runtimeID})
}

// ReadinessScopes checks each supplied scope.
func (r *Runner) ReadinessScopes(ctx context.Context, scopes []recall.Scope) (ReadinessResult, error) {
	if r.ready == nil {
		return ReadinessResult{}, validationf("memory does not implement ReadinessObserver")
	}
	scopes, err := normalizeScopes(scopes)
	if err != nil {
		return ReadinessResult{}, err
	}
	started := r.cfg.Now()
	out := ReadinessResult{
		StartedAt: started,
		Status:    recall.ReadinessReady,
		Reports:   make([]recall.ReadinessReport, 0, len(scopes)),
	}
	var failures []Failure
	for _, scope := range scopes {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		report, err := r.ready.Readiness(ctx, scope, r.cfg.ReadinessOptions)
		if err != nil {
			failures = append(failures, Failure{Scope: scope, Operation: "readiness", Err: err})
			continue
		}
		out.Reports = append(out.Reports, report)
		out.Status = worseReadiness(out.Status, report.Status)
		r.emit(ctx, Event{
			Time:      r.cfg.Now(),
			Kind:      EventReadiness,
			Scope:     report.Scope,
			Readiness: &report,
		})
	}
	out.FinishedAt = r.cfg.Now()
	if len(failures) > 0 {
		return out, Error{Failures: failures}
	}
	return out, nil
}

func worseReadiness(a, b recall.ReadinessStatus) recall.ReadinessStatus {
	if readinessRank(b) > readinessRank(a) {
		return b
	}
	return a
}

func readinessRank(s recall.ReadinessStatus) int {
	switch s {
	case recall.ReadinessReady:
		return 0
	case recall.ReadinessSkipped:
		return 1
	case recall.ReadinessDegraded:
		return 2
	case recall.ReadinessNotReady:
		return 3
	default:
		return 3
	}
}
