package evolution

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// ExpireRetired performs a one-shot sweep that hard-deletes facts whose
// ExpiresAt is before now. Callers invoke explicitly; recall does not run
// a background goroutine (Phase D.8).
func ExpireRetired(ctx context.Context, store port.TemporalStore, scope domain.Scope, now time.Time) ([]string, error) {
	facts, err := store.List(ctx, scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		return nil, err
	}
	var expired []string
	for _, f := range facts {
		if f.ExpiresAt != nil && !f.ExpiresAt.IsZero() && !now.Before(*f.ExpiresAt) {
			expired = append(expired, f.ID)
		}
	}
	if len(expired) == 0 {
		return nil, nil
	}
	if err := store.Delete(ctx, scope, expired); err != nil {
		return nil, err
	}
	return expired, nil
}
