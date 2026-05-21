package evidence

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Projection mirrors TemporalFact.EvidenceRefs into the secondary
// EvidenceStore. Required consistency: Append failures abort Save
// and trigger the write pipeline rollback (canonical delete +
// projection forget), matching plan §C.3.
type Projection struct {
	store port.EvidenceStore
}

// New constructs an evidence projection. store may be nil (all ops
// no-op); memory only registers this lens when a store is wired.
func New(store port.EvidenceStore) *Projection {
	return &Projection{store: store}
}

func (p *Projection) Name() string { return "evidence" }

func (p *Projection) Consistency() port.Consistency { return port.Required }

func (p *Projection) Project(ctx context.Context, facts []domain.TemporalFact) error {
	if p.store == nil || len(facts) == 0 {
		return nil
	}
	for _, f := range facts {
		if len(f.EvidenceRefs) == 0 {
			continue
		}
		if err := p.store.Append(ctx, f.Scope, f.ID, f.EvidenceRefs); err != nil {
			return fmt.Errorf("evidence append %s: %w", f.ID, err)
		}
	}
	return nil
}

func (p *Projection) Forget(ctx context.Context, scope domain.Scope, factIDs []string) error {
	if p.store == nil || len(factIDs) == 0 {
		return nil
	}
	return p.store.ForgetByFact(ctx, scope, factIDs)
}

// Rebuild applies exact-replace semantics: forget every adapter id
// then re-append from the canonical snapshot (see memory.rebuildEvidence).
func (p *Projection) Rebuild(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) error {
	if p.store == nil {
		return nil
	}
	adapterIDs, err := p.store.ListFactIDs(ctx, scope)
	if err != nil {
		return fmt.Errorf("list evidence fact ids: %w", err)
	}
	ids := make(map[string]struct{}, len(adapterIDs)+len(facts))
	for _, id := range adapterIDs {
		ids[id] = struct{}{}
	}
	for _, f := range facts {
		if f.ID != "" {
			ids[f.ID] = struct{}{}
		}
	}
	if len(ids) > 0 {
		toForget := make([]string, 0, len(ids))
		for id := range ids {
			toForget = append(toForget, id)
		}
		if err := p.store.ForgetByFact(ctx, scope, toForget); err != nil {
			return fmt.Errorf("forget evidence: %w", err)
		}
	}
	for _, f := range facts {
		if len(f.EvidenceRefs) == 0 {
			continue
		}
		if err := p.store.Append(ctx, scope, f.ID, f.EvidenceRefs); err != nil {
			return fmt.Errorf("append evidence %s: %w", f.ID, err)
		}
	}
	return nil
}

// ClearScope removes every evidence ref in the scope by enumerating
// fact ids from the store and issuing ForgetByFact. Backs
// Memory.ForgetAll (D.8 C9, GDPR mode = Hard).
func (p *Projection) ClearScope(ctx context.Context, scope domain.Scope) error {
	if p.store == nil {
		return nil
	}
	ids, err := p.store.ListFactIDs(ctx, scope)
	if err != nil {
		return fmt.Errorf("evidence clear list ids: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := p.store.ForgetByFact(ctx, scope, ids); err != nil {
		return fmt.Errorf("evidence clear forget: %w", err)
	}
	return nil
}

var _ port.Projection = (*Projection)(nil)
