package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

// ValidateEpisodesForJob ensures every EpisodeFactID still exists in
// the canonical store and is eligible for semantic derivation. Missing
// or retired episodes yield a permanent worker failure.
func ValidateEpisodesForJob(ctx context.Context, store port.TemporalStore, job port.AsyncSemanticJob, now time.Time) error {
	if store == nil {
		return fmt.Errorf("recall async semantic: store not configured")
	}
	if len(job.EpisodeFactIDs) == 0 && len(job.TurnsSnapshot) == 0 {
		return fmt.Errorf("recall async semantic: job has no episode ids or turn snapshot")
	}
	if len(job.EpisodeFactIDs) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	for _, id := range job.EpisodeFactIDs {
		f, err := store.Get(ctx, job.Scope, id)
		if err != nil {
			if err == temporalstore.ErrNotFound {
				return fmt.Errorf("episode fact %q not found", id)
			}
			return fmt.Errorf("episode fact %q: %w", id, err)
		}
		if f.Kind != domain.KindEpisode {
			return fmt.Errorf("fact %q kind=%s, want episode", id, f.Kind)
		}
		if !domain.IsCanonicalActive(f, now) {
			return fmt.Errorf("episode fact %q is not canonical-active", id)
		}
		if domain.IsRetired(f, now) {
			return fmt.Errorf("episode fact %q is retired", id)
		}
	}
	return nil
}
