package stages

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// ParseCanonicalTurns parses the "<speaker>: <text>" lines stored on
// KindEpisode facts by build_episode. Turn IDs are synthetic; prefer
// enqueue-time TurnsSnapshot when present (see ReconstructTurnsForJob).
func ParseCanonicalTurns(content string) []domain.TurnContext {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	out := make([]domain.TurnContext, 0, len(lines))
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		speaker, text, ok := strings.Cut(line, ": ")
		if !ok {
			text = line
			speaker = "unknown"
		}
		if speaker == "" {
			speaker = "unknown"
		}
		out = append(out, domain.TurnContext{
			ID:      fmt.Sprintf("turn-%d", i),
			Speaker: speaker,
			Text:    text,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ReconstructTurnsForJob prefers the enqueue-time TurnsSnapshot so
// worker extraction keeps stable turn IDs and full TurnContext fields.
// When the snapshot is absent it loads canonical episode content.
func ReconstructTurnsForJob(ctx context.Context, store port.TemporalStore, job port.AsyncSemanticJob) ([]domain.TurnContext, error) {
	if len(job.TurnsSnapshot) > 0 {
		return append([]domain.TurnContext(nil), job.TurnsSnapshot...), nil
	}
	if store != nil && len(job.EpisodeFactIDs) > 0 {
		var turns []domain.TurnContext
		for _, id := range job.EpisodeFactIDs {
			f, err := store.Get(ctx, job.Scope, id)
			if err != nil {
				return nil, fmt.Errorf("episode fact %q: %w", id, err)
			}
			if f.Kind != domain.KindEpisode {
				return nil, fmt.Errorf("fact %q kind=%s, want episode", id, f.Kind)
			}
			parsed := ParseCanonicalTurns(f.Content)
			turns = append(turns, parsed...)
		}
		if len(turns) > 0 {
			return turns, nil
		}
	}
	return nil, fmt.Errorf("recall async semantic: no turns from snapshot or episodes")
}
