package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
)

// EvidenceMirror writes EvidenceRefs into the secondary lookup
// store. The mirror is a rebuildable derived view — embedded
// EvidenceRefs on the canonical fact remain authoritative — so a
// failure here is reported via the legacy OnProjection channel and
// returned as a Status=failed diagnostic, but it does NOT propagate
// an error (matching the legacy `if evErr != nil { telemetry; }
// continue` behaviour).
//
// No compensator: there is nothing to undo when the secondary store
// either rejected the append or wasn't configured. project_required
// owns the secondary cleanup on rollback.
type EvidenceMirror struct {
	store port.EvidenceStore
	hook  port.TelemetryHook
}

// NewEvidenceMirror constructs the stage. store may be nil (no
// EvidenceStore configured); the stage emits an OK diagnostic with
// EventsRecorded=0 in that case.
func NewEvidenceMirror(store port.EvidenceStore, hook port.TelemetryHook) *EvidenceMirror {
	return &EvidenceMirror{store: store, hook: hook}
}

// Name implements pipeline.Stage.
func (EvidenceMirror) Name() string { return "evidence_mirror" }

// Run implements pipeline.Stage. When no EvidenceStore is wired
// the stage records a zero-event diagnostic — matching legacy
// runSave, which always emitted the "evidence/mirror" pipeline
// event regardless of whether an adapter was configured (the
// legacy mirrorEvidence helper returned nil immediately on a nil
// store, then runSave still emitted the OK telemetry).
func (s *EvidenceMirror) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	if s.store == nil {
		return diagnostic.EvidenceMirrorDetail{Latency: time.Since(started)}, nil
	}
	events, err := s.mirror(ctx, state.Scope, state.Resolution.Facts)
	detail := diagnostic.EvidenceMirrorDetail{
		EventsRecorded: events,
		Latency:        time.Since(started),
	}
	if err != nil {
		// Non-fatal: stash the error on state so the legacy
		// bridge can reconstruct the OnPipeline event, then
		// emit the legacy OnProjection inline (matching the
		// legacy runSave's two-channel emit). We return nil so
		// the pipeline framework does NOT trigger compensation
		// — the secondary mirror is rebuildable from the
		// canonical ledger.
		state.EvidenceMirrorErr = err
		s.emit(port.ProjectionEvent{
			Projection:  "evidence",
			Op:          port.OpProject,
			Consistency: projection.Optional.String(),
			FactCount:   len(state.Resolution.Facts),
			Err:         err,
		})
	}
	state.EvidenceMirrored = events
	return detail, nil
}

// mirror appends EvidenceRefs into the secondary lookup store, per
// fact. Append is idempotent on (scope, factID, refs[i].ID) so
// retries and rebuilds replay without producing duplicate entries.
func (s *EvidenceMirror) mirror(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) (int, error) {
	count := 0
	for _, f := range facts {
		if len(f.EvidenceRefs) == 0 {
			continue
		}
		if err := s.store.Append(ctx, scope, f.ID, f.EvidenceRefs); err != nil {
			return count, fmt.Errorf("evidence append %s: %w", f.ID, err)
		}
		count += len(f.EvidenceRefs)
	}
	return count, nil
}

func (s *EvidenceMirror) emit(ev port.ProjectionEvent) {
	if s.hook == nil {
		return
	}
	s.hook.OnProjection(ev)
}

var _ pipeline.Stage[*write.WriteState] = (*EvidenceMirror)(nil)
