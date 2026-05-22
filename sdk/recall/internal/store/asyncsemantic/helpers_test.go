package asyncsemantic

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func claimBatch(ctx context.Context, q *Queue, workerID string, now time.Time, max int) ([]port.AsyncSemanticJob, error) {
	return q.Claim(ctx, port.AsyncSemanticClaimOptions{
		WorkerID: workerID,
		Now:      now,
		Max:      max,
	})
}

func makeJob(requestID, user string, episodeIDs ...string) port.AsyncSemanticJob {
	return port.AsyncSemanticJob{
		RequestID:      requestID,
		Scope:          domain.Scope{RuntimeID: "rt", UserID: user},
		EpisodeFactIDs: episodeIDs,
	}
}
