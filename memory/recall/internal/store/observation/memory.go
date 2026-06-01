package observation

import (
	"context"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// MemoryStore is the reference in-memory ObservationStore.
type MemoryStore struct {
	mu      sync.RWMutex
	byScope map[scopeKey]*scopeShard
}

type scopeKey struct {
	runtimeID string
	userID    string
}

type scopeShard struct {
	byID       map[string]*domain.Observation
	orderedIDs []string
}

// New returns an empty in-memory observation ledger.
func New() *MemoryStore {
	return &MemoryStore{byScope: make(map[scopeKey]*scopeShard)}
}

func keyOf(s domain.Scope) scopeKey {
	return scopeKey{runtimeID: s.RuntimeID, userID: s.UserID}
}

func (s *MemoryStore) shardLocked(scope domain.Scope) *scopeShard {
	k := keyOf(scope)
	sh, ok := s.byScope[k]
	if !ok {
		sh = &scopeShard{byID: make(map[string]*domain.Observation)}
		s.byScope[k] = sh
	}
	return sh
}

// Append stores observations atomically. Existing identical IDs are treated as
// retry idempotency; conflicting duplicate IDs are rejected.
func (s *MemoryStore) Append(_ context.Context, observations []domain.Observation) error {
	if len(observations) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	stagedByScope := make(map[scopeKey]map[string]domain.Observation, len(observations))
	for _, o := range observations {
		if o.ID == "" {
			return errdefs.Validationf("recall observation store: observation id is required")
		}
		if o.Scope.RuntimeID == "" {
			return errdefs.Validationf("recall observation store: observation %q missing scope.runtime_id", o.ID)
		}
		k := keyOf(o.Scope)
		scopeStaged, ok := stagedByScope[k]
		if !ok {
			scopeStaged = make(map[string]domain.Observation)
			stagedByScope[k] = scopeStaged
		}
		if prev, ok := scopeStaged[o.ID]; ok {
			merged, _, conflict := domain.MergeObservation(prev, o)
			if conflict {
				return errdefs.Conflictf("recall observation store: duplicate observation id %q within append batch", o.ID)
			}
			scopeStaged[o.ID] = merged
			continue
		}
		scopeStaged[o.ID] = o.Clone()
	}

	type stagedObservation struct {
		scope domain.Scope
		obs   domain.Observation
	}
	inserts := make([]stagedObservation, 0, len(observations))
	updates := make([]stagedObservation, 0, len(observations))
	for _, byID := range stagedByScope {
		for _, o := range byID {
			sh := s.shardLocked(o.Scope)
			if existing, ok := sh.byID[o.ID]; ok {
				merged, changed, conflict := domain.MergeObservation(existing.Clone(), o)
				if conflict {
					return errdefs.Conflictf("recall observation store: duplicate observation id %q in scope", o.ID)
				}
				if changed {
					updates = append(updates, stagedObservation{scope: o.Scope, obs: merged})
				}
				continue
			}
			inserts = append(inserts, stagedObservation{scope: o.Scope, obs: o.Clone()})
		}
	}

	for _, item := range updates {
		sh := s.shardLocked(item.scope)
		stored := item.obs
		sh.byID[stored.ID] = &stored
	}
	for _, item := range inserts {
		sh := s.shardLocked(item.scope)
		stored := item.obs
		sh.byID[stored.ID] = &stored
		sh.orderedIDs = append(sh.orderedIDs, stored.ID)
	}
	return nil
}

func (s *MemoryStore) Get(_ context.Context, scope domain.Scope, observationID string) (domain.Observation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return domain.Observation{}, port.ErrNotFound
	}
	o, ok := sh.byID[observationID]
	if !ok {
		return domain.Observation{}, port.ErrNotFound
	}
	return o.Clone(), nil
}

func (s *MemoryStore) List(_ context.Context, scope domain.Scope, query port.ObservationListQuery) ([]domain.Observation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil, nil
	}

	kindSet := make(map[domain.ObservationKind]struct{}, len(query.Kinds))
	for _, k := range query.Kinds {
		kindSet[k] = struct{}{}
	}
	out := make([]domain.Observation, 0, len(sh.orderedIDs))
	for _, id := range sh.orderedIDs {
		o := sh.byID[id]
		if len(kindSet) > 0 {
			if _, ok := kindSet[o.Kind]; !ok {
				continue
			}
		}
		if query.SourceID != "" && o.SourceID != query.SourceID {
			continue
		}
		out = append(out, o.Clone())
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ObservedAt.Before(out[j].ObservedAt)
	})
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (s *MemoryStore) Delete(_ context.Context, scope domain.Scope, observationIDs []string) error {
	if len(observationIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil
	}
	remove := make(map[string]struct{}, len(observationIDs))
	for _, id := range observationIDs {
		if id == "" {
			continue
		}
		delete(sh.byID, id)
		remove[id] = struct{}{}
	}
	sh.orderedIDs = compactIDs(sh.orderedIDs, remove)
	return nil
}

func (s *MemoryStore) DeleteByScope(_ context.Context, scope domain.Scope) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := keyOf(scope)
	sh, ok := s.byScope[k]
	if !ok {
		return 0, nil
	}
	n := len(sh.byID)
	delete(s.byScope, k)
	return n, nil
}

func (s *MemoryStore) Close() error { return nil }

func compactIDs(ids []string, remove map[string]struct{}) []string {
	if len(remove) == 0 {
		return ids
	}
	out := ids[:0]
	for _, id := range ids {
		if _, drop := remove[id]; drop {
			continue
		}
		out = append(out, id)
	}
	return out
}

var _ port.ObservationStore = (*MemoryStore)(nil)
