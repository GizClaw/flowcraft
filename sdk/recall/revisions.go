package recall

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/revision"
)

var ErrCrossScope = errdefs.Forbidden(errdefs.New("recall: revision crosses scope boundary"))

// Fork appends a parallel revision via the revision pipeline.
func (m *memory) Fork(ctx context.Context, scope Scope, sourceFactID string, newFact TemporalFact) (SaveResult, error) {
	return m.runRevision(ctx, &revision.State{Scope: scope, Mode: revision.ModeFork, SourceFactID: sourceFactID, NewFact: newFact})
}

// Contest records a challenge note via the revision pipeline.
func (m *memory) Contest(ctx context.Context, scope Scope, factID string, evidence []EvidenceRef) (SaveResult, error) {
	return m.runRevision(ctx, &revision.State{Scope: scope, Mode: revision.ModeContest, SourceFactID: factID, Evidence: evidence})
}

func (m *memory) runRevision(ctx context.Context, st *revision.State) (SaveResult, error) {
	unlock := m.lockWriteScope(st.Scope)
	defer unlock()
	if err := m.revisionRunner.Run(ctx, st); err != nil {
		return SaveResult{}, err
	}
	return SaveResult{FactIDs: []string{st.Created.ID}}, nil
}
