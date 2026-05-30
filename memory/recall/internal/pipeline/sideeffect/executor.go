// Package sideeffect executes commit-after outbox jobs outside the
// scope write lock.
package sideeffect

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/lens/retrieval"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// Executor runs SideEffectOutbox jobs against the wired fanout,
// evolution runner, and retrieval projection.
type Executor struct {
	Fanout      *pipeline.Fanout
	Projections []port.Projection
	Evolution   port.EvolutionRunner
	Retrieval   *retrieval.Projection
	Telemetry   port.TelemetryHook
}

// Run implements port.SideEffectExecutor.
func (e *Executor) Run(ctx context.Context, job port.SideEffectJob) error {
	if e == nil {
		return nil
	}
	switch job.Kind {
	case port.SideEffectProjectRequired:
		if e.Fanout == nil {
			return nil
		}
		started := time.Now()
		err := e.Fanout.ProjectRequired(ctx, job.Facts)
		e.emitProject("project_required", "required", len(job.Facts), started, err)
		return err
	case port.SideEffectProjectOptional:
		if e.Fanout == nil {
			return nil
		}
		started := time.Now()
		e.Fanout.ProjectOptional(ctx, job.Facts)
		e.emitProject("project_optional", "optional", len(job.Facts), started, nil)
		return nil
	case port.SideEffectProjectEpisodeEvidence:
		if e.Fanout == nil || len(job.Facts) == 0 {
			return nil
		}
		started := time.Now()
		err := e.Fanout.ProjectRequiredForKindsStrict(ctx, job.Facts, domain.KindEpisode)
		status := diagnostic.StatusOK
		errMsg := ""
		if err != nil {
			status = diagnostic.StatusFailed
			errMsg = err.Error()
		}
		e.emit(diagnostic.StageDiagnostic{
			Stage:          "project_episode_evidence",
			Phase:          diagnostic.PhaseWrite,
			StartAt:        started,
			Duration:       time.Since(started),
			Status:         status,
			Err:            errMsg,
			AsyncRequestID: job.RequestID,
			Detail: diagnostic.ProjectEpisodeEvidenceDetail{
				AsyncRequestID: job.RequestID,
				EpisodeFacts:   len(job.Facts),
				Latency:        time.Since(started),
			},
		})
		return err
	case port.SideEffectEmbeddingBackfill:
		if e.Retrieval == nil || len(job.Facts) == 0 {
			return nil
		}
		return e.Retrieval.BackfillEmbeddings(ctx, job.Facts)
	case port.SideEffectEvolutionAfterSave:
		if e.Evolution == nil {
			return nil
		}
		ids := factIDs(job.Facts)
		if len(ids) == 0 {
			return nil
		}
		return e.Evolution.AfterSave(ctx, job.Scope, ids)
	default:
		return nil
	}
}

func (e *Executor) emitProject(stage, consistency string, applied int, started time.Time, err error) {
	status := diagnostic.StatusOK
	errMsg := ""
	if err != nil {
		status = diagnostic.StatusFailed
		errMsg = err.Error()
	}
	e.emit(diagnostic.StageDiagnostic{
		Stage:    stage,
		Phase:    diagnostic.PhaseWrite,
		StartAt:  started,
		Duration: time.Since(started),
		Status:   status,
		Err:      errMsg,
		Detail: diagnostic.ProjectDetail{
			Consistency: consistency,
			Results: []diagnostic.ProjectionResult{{
				Applied: applied,
				Latency: time.Since(started),
				Err:     errMsg,
			}},
		},
	})
}

func (e *Executor) emit(d diagnostic.StageDiagnostic) {
	if e == nil || e.Telemetry == nil {
		return
	}
	e.Telemetry.OnStage(d)
}

func factIDs(facts []domain.TemporalFact) []string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		if f.ID != "" {
			out = append(out, f.ID)
		}
	}
	return out
}

var _ port.SideEffectExecutor = (*Executor)(nil)
