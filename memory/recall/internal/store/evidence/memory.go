package evidence

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// MemoryStore is the reference in-memory EvidenceStore shipped with
// PR-5. Durable backends (jsonl/sqlite) land in later phases without
// changing the Store boundary.
type MemoryStore struct {
	mu      sync.RWMutex
	byScope map[scopeKey]*scopeShard
}

// scopeKey mirrors the temporal store's soft-isolation policy:
// (runtime, user) is the hard partition, AgentID is metadata.
type scopeKey struct {
	runtimeID string
	userID    string
}

type scopeShard struct {
	byID   map[string]domain.EvidenceRef
	byFact map[string][]string
}

// NewMemoryStore returns an empty in-memory EvidenceStore.
func NewMemoryStore() *MemoryStore {
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
			byID:   make(map[string]domain.EvidenceRef),
			byFact: make(map[string][]string),
		}
		s.byScope[k] = sh
	}
	return sh
}

// Append mirrors refs into the lookup store. Idempotent: replaying
// the same (scope, factID, refs) produces the same ids and does not
// duplicate entries. Refs with empty ID get auto-assigned a stable
// "<factID>#<index>" id so rebuild/rollback retries stay safe.
func (s *MemoryStore) Append(_ context.Context, scope domain.Scope, factID string, refs []domain.EvidenceRef) error {
	if scope.RuntimeID == "" {
		return errdefs.Validationf("recall evidence store: scope.runtime_id is required")
	}
	if factID == "" {
		return errdefs.Validationf("recall evidence store: fact id is required")
	}
	if len(refs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sh := s.shardLocked(scope)
	existing := make(map[string]struct{}, len(sh.byFact[factID]))
	for _, id := range sh.byFact[factID] {
		existing[id] = struct{}{}
	}
	for i, r := range refs {
		if r.ID == "" {
			r.ID = fmt.Sprintf("%s#%d", factID, i)
		}
		if _, dup := existing[r.ID]; dup {
			// idempotent replay: overwrite payload but do not
			// duplicate the byFact index entry.
			sh.byID[evidenceStoreKey(factID, r.ID)] = r
			continue
		}
		sh.byID[evidenceStoreKey(factID, r.ID)] = r
		sh.byFact[factID] = append(sh.byFact[factID], r.ID)
		existing[r.ID] = struct{}{}
	}
	return nil
}

// Get returns one EvidenceRef. ErrNotFound when missing.
func (s *MemoryStore) Get(_ context.Context, scope domain.Scope, evidenceID string) (domain.EvidenceRef, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return domain.EvidenceRef{}, ErrNotFound
	}
	var found *domain.EvidenceRef
	for factID, ids := range sh.byFact {
		for _, id := range ids {
			if id != evidenceID {
				continue
			}
			r, ok := sh.byID[evidenceStoreKey(factID, id)]
			if !ok {
				continue
			}
			if found != nil {
				return domain.EvidenceRef{}, ErrAmbiguous
			}
			ref := r
			found = &ref
		}
	}
	if found != nil {
		return *found, nil
	}
	return domain.EvidenceRef{}, ErrNotFound
}

// ListFactIDs enumerates every fact id with at least one ref in
// this scope. Order is unspecified; callers treat the result as a
// set. Returns nil when the scope has no shard yet.
func (s *MemoryStore) ListFactIDs(_ context.Context, scope domain.Scope) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok || len(sh.byFact) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(sh.byFact))
	for id := range sh.byFact {
		out = append(out, id)
	}
	return out, nil
}

// ListByFact returns refs in append order. Empty factID returns nil
// so callers cannot accidentally enumerate the whole scope.
func (s *MemoryStore) ListByFact(_ context.Context, scope domain.Scope, factID string) ([]domain.EvidenceRef, error) {
	if factID == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil, nil
	}
	ids := sh.byFact[factID]
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]domain.EvidenceRef, 0, len(ids))
	for _, id := range ids {
		if r, ok := sh.byID[evidenceStoreKey(factID, id)]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// ForgetByFact removes all evidence attached to the listed facts.
// Missing fact ids are tolerated so partial-failure retries stay
// idempotent.
func (s *MemoryStore) ForgetByFact(_ context.Context, scope domain.Scope, factIDs []string) error {
	if len(factIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil
	}
	for _, fid := range factIDs {
		ids, ok := sh.byFact[fid]
		if !ok {
			continue
		}
		for _, id := range ids {
			delete(sh.byID, evidenceStoreKey(fid, id))
		}
		delete(sh.byFact, fid)
	}
	return nil
}

func evidenceStoreKey(factID, evidenceID string) string {
	return factID + "\x00" + evidenceID
}

// Close releases backend resources.
func (s *MemoryStore) Close() error { return nil }

var _ port.EvidenceStore = (*MemoryStore)(nil)
