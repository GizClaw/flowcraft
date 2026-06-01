package link

import (
	"context"
	"reflect"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// MemoryStore is the reference in-memory FactLink store.
type MemoryStore struct {
	mu      sync.RWMutex
	byScope map[scopeKey]*scopeShard
}

type scopeKey struct {
	runtimeID string
	userID    string
}

type scopeShard struct {
	byID        map[string]*domain.FactLink
	orderedIDs  []string
	mergeKeyIdx map[string]string
}

// New returns an empty in-memory link ledger.
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
		sh = &scopeShard{
			byID:        make(map[string]*domain.FactLink),
			mergeKeyIdx: make(map[string]string),
		}
		s.byScope[k] = sh
	}
	return sh
}

// Append stores links atomically. MergeKey is an idempotency key: an existing
// equal MergeKey skips the new link even if the generated ID differs.
func (s *MemoryStore) Append(_ context.Context, links []domain.FactLink) error {
	if len(links) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	staged := make([]domain.FactLink, 0, len(links))
	seenIDs := make(map[scopeKey]map[string]domain.FactLink, len(links))
	seenMergeKeys := make(map[scopeKey]map[string]struct{}, len(links))
	for _, l := range links {
		if err := validateLink(l); err != nil {
			return err
		}
		k := keyOf(l.Scope)
		if l.MergeKey != "" {
			scopeSeen, ok := seenMergeKeys[k]
			if !ok {
				scopeSeen = make(map[string]struct{})
				seenMergeKeys[k] = scopeSeen
			}
			if _, ok := scopeSeen[l.MergeKey]; ok {
				continue
			}
			scopeSeen[l.MergeKey] = struct{}{}
		}
		scopeSeenIDs, ok := seenIDs[k]
		if !ok {
			scopeSeenIDs = make(map[string]domain.FactLink)
			seenIDs[k] = scopeSeenIDs
		}
		if prev, ok := scopeSeenIDs[l.ID]; ok {
			if !reflect.DeepEqual(prev.Clone(), l.Clone()) {
				return errdefs.Conflictf("recall link store: duplicate link id %q within append batch", l.ID)
			}
			continue
		}
		scopeSeenIDs[l.ID] = l.Clone()

		sh := s.shardLocked(l.Scope)
		if l.MergeKey != "" {
			if _, exists := sh.mergeKeyIdx[l.MergeKey]; exists {
				continue
			}
		}
		if existing, ok := sh.byID[l.ID]; ok {
			if !reflect.DeepEqual(existing.Clone(), l.Clone()) {
				return errdefs.Conflictf("recall link store: duplicate link id %q in scope", l.ID)
			}
			continue
		}
		staged = append(staged, l.Clone())
	}

	for _, l := range staged {
		sh := s.shardLocked(l.Scope)
		stored := l
		sh.byID[stored.ID] = &stored
		sh.orderedIDs = append(sh.orderedIDs, stored.ID)
		if stored.MergeKey != "" {
			sh.mergeKeyIdx[stored.MergeKey] = stored.ID
		}
	}
	return nil
}

func validateLink(l domain.FactLink) error {
	if l.ID == "" {
		return errdefs.Validationf("recall link store: link id is required")
	}
	if l.Scope.RuntimeID == "" {
		return errdefs.Validationf("recall link store: link %q missing scope.runtime_id", l.ID)
	}
	if l.Type == "" {
		return errdefs.Validationf("recall link store: link %q missing type", l.ID)
	}
	if l.From.Kind == "" || l.From.ID == "" || l.To.Kind == "" || l.To.ID == "" {
		return errdefs.Validationf("recall link store: link %q requires from/to refs", l.ID)
	}
	return nil
}

func (s *MemoryStore) Get(_ context.Context, scope domain.Scope, linkID string) (domain.FactLink, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return domain.FactLink{}, port.ErrNotFound
	}
	l, ok := sh.byID[linkID]
	if !ok {
		return domain.FactLink{}, port.ErrNotFound
	}
	return l.Clone(), nil
}

func (s *MemoryStore) List(_ context.Context, scope domain.Scope, query port.LinkListQuery) ([]domain.FactLink, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil, nil
	}

	typeSet := make(map[domain.FactLinkType]struct{}, len(query.Types))
	for _, typ := range query.Types {
		typeSet[typ] = struct{}{}
	}
	out := make([]domain.FactLink, 0, len(sh.orderedIDs))
	for _, id := range sh.orderedIDs {
		l := sh.byID[id]
		if len(typeSet) > 0 {
			if _, ok := typeSet[l.Type]; !ok {
				continue
			}
		}
		if !nodeRefMatches(query.From, l.From) || !nodeRefMatches(query.To, l.To) {
			continue
		}
		out = append(out, l.Clone())
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (s *MemoryStore) FindByNode(ctx context.Context, scope domain.Scope, node domain.GraphNodeRef) ([]domain.FactLink, error) {
	if node.Kind == "" || node.ID == "" {
		return nil, nil
	}
	links, err := s.List(ctx, scope, port.LinkListQuery{})
	if err != nil {
		return nil, err
	}
	out := make([]domain.FactLink, 0, len(links))
	for _, l := range links {
		if l.From == node || l.To == node {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *MemoryStore) FindByMergeKey(_ context.Context, scope domain.Scope, mergeKey string) ([]domain.FactLink, error) {
	if mergeKey == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil, nil
	}
	id, ok := sh.mergeKeyIdx[mergeKey]
	if !ok {
		return nil, nil
	}
	l, ok := sh.byID[id]
	if !ok {
		return nil, nil
	}
	return []domain.FactLink{l.Clone()}, nil
}

func (s *MemoryStore) Delete(_ context.Context, scope domain.Scope, linkIDs []string) error {
	if len(linkIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil
	}
	remove := make(map[string]struct{}, len(linkIDs))
	for _, id := range linkIDs {
		if id == "" {
			continue
		}
		if l, ok := sh.byID[id]; ok && l.MergeKey != "" {
			delete(sh.mergeKeyIdx, l.MergeKey)
		}
		delete(sh.byID, id)
		remove[id] = struct{}{}
	}
	sh.orderedIDs = compactIDs(sh.orderedIDs, remove)
	return nil
}

func (s *MemoryStore) DeleteByNode(_ context.Context, scope domain.Scope, node domain.GraphNodeRef) (int, error) {
	if node.Kind == "" || node.ID == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return 0, nil
	}
	remove := make(map[string]struct{})
	for id, l := range sh.byID {
		if l.From == node || l.To == node {
			remove[id] = struct{}{}
			if l.MergeKey != "" {
				delete(sh.mergeKeyIdx, l.MergeKey)
			}
			delete(sh.byID, id)
		}
	}
	sh.orderedIDs = compactIDs(sh.orderedIDs, remove)
	return len(remove), nil
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

func nodeRefMatches(query, actual domain.GraphNodeRef) bool {
	if query.Kind != "" && query.Kind != actual.Kind {
		return false
	}
	if query.ID != "" && query.ID != actual.ID {
		return false
	}
	return true
}

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

var _ port.LinkStore = (*MemoryStore)(nil)
