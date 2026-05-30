package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/revision"
)

// AttachRevision is the second revision pipeline stage. It mutates
// state.NewFact in-place so revision_save sees the final shape the
// canonical store will receive:
//
//   - ModeFork: state.NewFact carries the caller's draft. The stage
//     stamps Scope, defaults MergeKey to `<source.MergeKey>:fork`
//     when empty, and attaches Revision{Kind: RevisionFork,
//     SourceFactID}. The draft's existing ObservedAt is preserved.
//   - ModeContest: state.NewFact is overwritten with a fresh
//     FactNote carrying state.Note as content, the supplied
//     EvidenceRefs, and Revision{Kind: RevisionContest,
//     SourceFactID}. ObservedAt defaults to time.Now() when zero.
type AttachRevision struct{}

// NewAttachRevision constructs the stage.
func NewAttachRevision() *AttachRevision { return &AttachRevision{} }

// Name implements pipeline.Stage.
func (AttachRevision) Name() string { return "revision_attach" }

// Run implements pipeline.Stage.
func (s *AttachRevision) Run(_ context.Context, state *revision.State) (diagnostic.StageDetail, error) {
	started := time.Now()
	detail := diagnostic.RevisionDetail{
		Kind:         state.Mode.KindString(),
		Stage:        "revision_attach",
		SourceFactID: state.SourceFactID,
	}

	switch state.Mode {
	case revision.ModeFork:
		fact := state.NewFact
		fact.Scope = state.Scope
		if fact.MergeKey == "" && state.Source.MergeKey != "" {
			fact.MergeKey = state.Source.MergeKey + ":fork"
		}
		domain.AttachRevision(&fact, domain.Revision{
			Kind:         domain.RevisionFork,
			SourceFactID: state.SourceFactID,
		})
		state.NewFact = fact
	case revision.ModeContest:
		contest := domain.TemporalFact{
			Scope:        state.Scope,
			Kind:         domain.KindNote,
			Content:      state.Note,
			EvidenceRefs: append([]domain.EvidenceRef(nil), state.Evidence...),
			ObservedAt:   time.Now(),
		}
		if contest.Content == "" {
			contest.Content = fmt.Sprintf("contest of %s", state.SourceFactID)
		}
		domain.AttachRevision(&contest, domain.Revision{
			Kind:         domain.RevisionContest,
			SourceFactID: state.SourceFactID,
		})
		state.NewFact = contest
	default:
		detail.Latency = time.Since(started)
		return detail, fmt.Errorf("recall.Revision: unknown mode %d", int(state.Mode))
	}
	detail.Latency = time.Since(started)
	return detail, nil
}

var _ pipeline.Stage[*revision.State] = (*AttachRevision)(nil)
