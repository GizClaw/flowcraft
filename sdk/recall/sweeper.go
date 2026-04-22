package recall

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// TTLPolicy decides default ExpiresAt for facts during extraction.
type TTLPolicy interface {
	TTLFor(entry MemoryEntry) (ttl time.Duration, ok bool)
}

// CategoryTTLPolicy maps category → TTL; missing keys mean "never expires".
type CategoryTTLPolicy map[MemoryCategory]time.Duration

// TTLFor implements TTLPolicy.
func (p CategoryTTLPolicy) TTLFor(e MemoryEntry) (time.Duration, bool) {
	if d, ok := p[e.Category]; ok {
		return d, true
	}
	for _, c := range e.Categories {
		if d, ok := p[MemoryCategory(c)]; ok {
			return d, true
		}
	}
	return 0, false
}

// DefaultCategoryTTL returns the recommended baseline mapping.
func DefaultCategoryTTL() CategoryTTLPolicy {
	return CategoryTTLPolicy{
		CategoryEpisodic:   90 * 24 * time.Hour,
		CategoryProcedural: 180 * 24 * time.Hour,
	}
}

// -----------------------------------------------------------------------------
// Sweeper loop — owned by *lt
// -----------------------------------------------------------------------------

func (m *lt) sweeperLoop() {
	defer m.wgWorkers.Done()
	t := time.NewTicker(m.cfg.SweeperInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-t.C:
			_ = m.SweepOnce(context.Background())
		}
	}
}

// SweepOnce performs one round of TTL cleanup; exposed for tests.
//
// Strategy:
//  1. Try DeletableByFilter.DeleteByFilter once.
//  2. Otherwise List(Range{expires_at:{Lte:now}}) → Delete(ids) in batches.
func (m *lt) SweepOnce(ctx context.Context) error {
	now := m.cfg.Now().UnixMilli()
	filter := retrieval.Filter{
		And: []retrieval.Filter{
			{Exists: []string{"expires_at"}},
			{Range: map[string]retrieval.Range{"expires_at": {Lte: now}}},
		},
	}
	// We do not enumerate every namespace; SweepNamespace covers explicit ones.
	// In practice callers run a per-namespace sweeper or rely on adapter-side cron.
	return m.sweepNamespace(ctx, "", filter)
}

// SweepNamespace exposes a per-namespace TTL pass for callers who manage many
// namespaces (e.g. one per tenant).
func (m *lt) SweepNamespace(ctx context.Context, ns string) error {
	now := m.cfg.Now().UnixMilli()
	filter := retrieval.Filter{
		And: []retrieval.Filter{
			{Exists: []string{"expires_at"}},
			{Range: map[string]retrieval.Range{"expires_at": {Lte: now}}},
		},
	}
	return m.sweepNamespace(ctx, ns, filter)
}

func (m *lt) sweepNamespace(ctx context.Context, ns string, filter retrieval.Filter) error {
	if ns == "" {
		return nil
	}
	if d, ok := m.idx.(retrieval.DeletableByFilter); ok && m.idx.Capabilities().NativeDeleteByFilter {
		_, err := d.DeleteByFilter(ctx, ns, filter)
		return err
	}
	tok := ""
	for {
		page, err := m.idx.List(ctx, ns, retrieval.ListRequest{
			Filter:    filter,
			PageSize:  m.cfg.SweeperBatchMax,
			PageToken: tok,
		})
		if err != nil {
			return err
		}
		if len(page.Items) == 0 {
			return nil
		}
		ids := make([]string, 0, len(page.Items))
		for _, d := range page.Items {
			ids = append(ids, d.ID)
		}
		if err := m.idx.Delete(ctx, ns, ids); err != nil {
			return err
		}
		if page.NextPageToken == "" {
			return nil
		}
		tok = page.NextPageToken
	}
}
