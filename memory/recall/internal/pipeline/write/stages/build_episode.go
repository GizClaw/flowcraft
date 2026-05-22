package stages

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
)

// BuildEpisode translates SaveRequest.Turns into a single
// KindEpisode TemporalFact, stamps state.AsyncRequestID, and stages
// the fact on state.EpisodeFacts for the subsequent append /
// project / outbox stages.
//
// F.1a deliberately collapses every turn in one Save into one
// episode fact (canonical join of "<speaker>: <text>" lines). Per-
// turn or per-session splitting is left as a later refinement;
// keeping the lane single-fact today minimises the projection /
// resolver surface the episode kind has to defend against.
type BuildEpisode struct{}

// NewBuildEpisode returns a stateless BuildEpisode stage instance.
func NewBuildEpisode() *BuildEpisode { return &BuildEpisode{} }

// Name implements pipeline.Stage.
func (BuildEpisode) Name() string { return "build_episode" }

// Run implements pipeline.Stage.
func (s *BuildEpisode) Run(_ context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	turns := state.Turns
	now := time.Now()

	requestID := newAsyncRequestID(now)
	state.AsyncRequestID = requestID

	observedAt := state.ObservedAt
	if observedAt.IsZero() {
		observedAt = latestTurnTime(turns)
	}
	if observedAt.IsZero() {
		observedAt = now
	}
	validFrom := observedAt

	fact := domain.TemporalFact{
		ID:               newEpisodeFactID(requestID, now),
		Scope:            state.Scope,
		Kind:             domain.KindEpisode,
		Content:          canonicalRenderTurns(turns),
		EvidenceRefs:     refsFromTurns(turns),
		SourceMessageIDs: idsFromTurns(turns),
		ObservedAt:       observedAt,
		ValidFrom:        &validFrom,
		Origin: domain.FactOrigin{
			RequestID: requestID,
			Kind:      domain.OriginKindEpisode,
		},
	}
	state.EpisodeFacts = []domain.TemporalFact{fact}

	return diagnostic.BuildEpisodeDetail{
		Turns:          len(turns),
		EpisodeFacts:   1,
		AsyncRequestID: requestID,
	}, nil
}

// canonicalRenderTurns joins turn speakers and text into the body
// the episode fact stores. The format intentionally matches the LLM
// extractor's user-message JSONL semantics in spirit (speaker prefix,
// one turn per line) so the async worker can re-render without
// needing a second source of truth.
func canonicalRenderTurns(turns []domain.TurnContext) string {
	if len(turns) == 0 {
		return ""
	}
	var b strings.Builder
	for i, t := range turns {
		if i > 0 {
			b.WriteByte('\n')
		}
		speaker := t.Speaker
		if speaker == "" {
			speaker = t.Role
		}
		if speaker == "" {
			speaker = "unknown"
		}
		b.WriteString(speaker)
		b.WriteString(": ")
		b.WriteString(t.Text)
	}
	return b.String()
}

// refsFromTurns lifts each turn that carries a non-empty EvidenceID
// into an EvidenceRef anchored on (id, message_id, role, time, text).
// Turns without EvidenceID are skipped — the episode fact's Content
// still captures their text through canonicalRenderTurns.
func refsFromTurns(turns []domain.TurnContext) []domain.EvidenceRef {
	if len(turns) == 0 {
		return nil
	}
	refs := make([]domain.EvidenceRef, 0, len(turns))
	for _, t := range turns {
		if t.EvidenceID == "" {
			continue
		}
		refs = append(refs, domain.EvidenceRef{
			ID:        t.EvidenceID,
			MessageID: t.ID,
			Role:      t.Role,
			Text:      t.Text,
			Timestamp: t.Time,
		})
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

// idsFromTurns collects non-empty turn IDs into SourceMessageIDs so
// downstream lineage / lookup paths can resolve back to the original
// message rows.
func idsFromTurns(turns []domain.TurnContext) []string {
	if len(turns) == 0 {
		return nil
	}
	ids := make([]string, 0, len(turns))
	for _, t := range turns {
		if t.ID == "" {
			continue
		}
		ids = append(ids, t.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// latestTurnTime returns the largest non-zero Time across the input
// turns. Falls back to zero when no turn carries a timestamp; the
// caller then defaults to time.Now().
func latestTurnTime(turns []domain.TurnContext) time.Time {
	var out time.Time
	for _, t := range turns {
		if t.Time.IsZero() {
			continue
		}
		if t.Time.After(out) {
			out = t.Time
		}
	}
	return out
}

// newAsyncRequestID generates a sortable opaque key for one async
// semantic work item. The "areq_" prefix lets logs distinguish it
// from canonical fact IDs at a glance; the millisecond timestamp
// keeps the ID time-ordered for FIFO claim semantics, and the
// random suffix avoids collisions within the same millisecond.
func newAsyncRequestID(now time.Time) string {
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	return fmt.Sprintf("areq_%013d_%s", now.UnixMilli(), hex.EncodeToString(rnd[:]))
}

// newEpisodeFactID assigns the synchronous episode fact a stable ID
// derived from the AsyncRequestID so operators can scan ledger rows
// and correlate them with the durable work item without a join.
func newEpisodeFactID(requestID string, now time.Time) string {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	return fmt.Sprintf("epi_%013d_%s_%s", now.UnixMilli(), requestID, hex.EncodeToString(rnd[:]))
}

var _ pipeline.Stage[*write.WriteState] = (*BuildEpisode)(nil)
