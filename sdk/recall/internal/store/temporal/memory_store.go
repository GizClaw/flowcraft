package temporal

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// MemoryStore is the reference in-memory TemporalFactStore.
//
// It is the reference port.TemporalStore implementation shipped with
// PR-2; durable backends (jsonl/sqlite) land in later phases without
// altering the port contract or its append-first semantics.
type MemoryStore struct {
	mu      sync.RWMutex
	byScope map[scopeKey]*scopeShard
}

type scopeShard struct {
	byID         map[string]*domain.TemporalFact
	orderedIDs   []string
	mergeKeyIdx  map[string][]string
	correctedIdx map[string][]string
}

// scopeKey is the canonical partition key. AgentID is intentionally
// excluded: per docs §5.1 it is a soft-isolation dimension surfaced
// through metadata/filters, not a hard partition. Including it would
// fragment a single user's ledger by agent and break cross-agent
// recall.
type scopeKey struct {
	runtimeID string
	userID    string
}

// NewMemoryStore returns an empty in-memory TemporalFactStore.
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
			byID:         make(map[string]*domain.TemporalFact),
			mergeKeyIdx:  make(map[string][]string),
			correctedIdx: make(map[string][]string),
		}
		s.byScope[k] = sh
	}
	return sh
}

// Append validates and stores facts atomically. Each fact must carry
// a non-empty ID and a valid FactKind. Duplicate IDs are rejected
// against both already-stored facts AND other facts within the same
// batch, so a partial commit is not possible: either every fact in
// the batch is appended, or none.
func (s *MemoryStore) Append(_ context.Context, facts []domain.TemporalFact) error {
	if len(facts) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	staged := make([]domain.TemporalFact, 0, len(facts))
	// batchSeen tracks ids reserved within this batch per scope, so
	// the validation phase catches both (already-stored, batch-new)
	// and (batch-new, batch-new) duplicate ids before we touch any
	// shard state.
	batchSeen := make(map[scopeKey]map[string]struct{}, len(facts))
	for _, f := range facts {
		if f.ID == "" {
			return errdefs.Validationf("recall temporal store: fact id is required")
		}
		if !f.Kind.IsValid() {
			return errdefs.Validationf("recall temporal store: invalid fact kind %q for fact %q", f.Kind, f.ID)
		}
		if f.Scope.RuntimeID == "" {
			return errdefs.Validationf("recall temporal store: fact %q missing scope.runtime_id", f.ID)
		}
		sh := s.shardLocked(f.Scope)
		if _, exists := sh.byID[f.ID]; exists {
			return errdefs.Conflictf("recall temporal store: duplicate fact id %q in scope", f.ID)
		}
		k := keyOf(f.Scope)
		seen, ok := batchSeen[k]
		if !ok {
			seen = make(map[string]struct{})
			batchSeen[k] = seen
		}
		if _, dup := seen[f.ID]; dup {
			return errdefs.Conflictf("recall temporal store: duplicate fact id %q within append batch", f.ID)
		}
		seen[f.ID] = struct{}{}
		staged = append(staged, f.Clone())
	}

	for _, f := range staged {
		sh := s.shardLocked(f.Scope)
		stored := f
		sh.byID[stored.ID] = &stored
		sh.orderedIDs = append(sh.orderedIDs, stored.ID)
		if stored.MergeKey != "" {
			sh.mergeKeyIdx[stored.MergeKey] = append(sh.mergeKeyIdx[stored.MergeKey], stored.ID)
		}
		if stored.CorrectedBy != "" {
			sh.correctedIdx[stored.CorrectedBy] = append(sh.correctedIdx[stored.CorrectedBy], stored.ID)
		}
	}
	return nil
}

// Get returns a clone so callers cannot mutate stored facts.
func (s *MemoryStore) Get(_ context.Context, scope domain.Scope, factID string) (domain.TemporalFact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return domain.TemporalFact{}, ErrNotFound
	}
	f, ok := sh.byID[factID]
	if !ok {
		return domain.TemporalFact{}, ErrNotFound
	}
	return f.Clone(), nil
}

// List returns ObservedAt-ascending facts filtered by the supplied
// query. The default view hides superseded facts.
func (s *MemoryStore) List(_ context.Context, scope domain.Scope, query port.ListQuery) ([]domain.TemporalFact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil, nil
	}

	kindSet := make(map[domain.FactKind]struct{}, len(query.Kinds))
	for _, k := range query.Kinds {
		kindSet[k] = struct{}{}
	}

	out := make([]domain.TemporalFact, 0, len(sh.orderedIDs))
	for _, id := range sh.orderedIDs {
		f := sh.byID[id]
		if !query.IncludeSuperseded && isSuperseded(f) {
			continue
		}
		if len(kindSet) > 0 {
			if _, ok := kindSet[f.Kind]; !ok {
				continue
			}
		}
		if !hasAllEntities(f, query.Entities) {
			continue
		}
		out = append(out, f.Clone())
		if query.Limit > 0 && len(out) >= query.Limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ObservedAt.Before(out[j].ObservedAt)
	})
	return out, nil
}

// FindByMergeKey returns facts in append order. Empty mergeKey yields
// an empty result so callers cannot accidentally enumerate the whole
// scope.
func (s *MemoryStore) FindByMergeKey(_ context.Context, scope domain.Scope, mergeKey string) ([]domain.TemporalFact, error) {
	if mergeKey == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil, nil
	}
	ids := sh.mergeKeyIdx[mergeKey]
	out := make([]domain.TemporalFact, 0, len(ids))
	for _, id := range ids {
		if f, ok := sh.byID[id]; ok {
			out = append(out, f.Clone())
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ObservedAt.Before(out[j].ObservedAt)
	})
	return out, nil
}

func (s *MemoryStore) FindSupersededBy(_ context.Context, scope domain.Scope, factID string) ([]domain.TemporalFact, error) {
	if factID == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil, nil
	}
	ids := sh.correctedIdx[factID]
	out := make([]domain.TemporalFact, 0, len(ids))
	for _, id := range ids {
		if f, ok := sh.byID[id]; ok {
			out = append(out, f.Clone())
		}
	}
	return out, nil
}

// UpdateValidity closes a fact's validity window. The operation is
// idempotent: a fact already closed with the supplied (validTo,
// correctedBy) tuple returns nil. Any other re-close attempt returns
// an error so callers do not silently overwrite history.
func (s *MemoryStore) UpdateValidity(_ context.Context, scope domain.Scope, factID string, validTo time.Time, correctedBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return ErrNotFound
	}
	f, ok := sh.byID[factID]
	if !ok {
		return ErrNotFound
	}
	if f.ValidTo != nil {
		if f.ValidTo.Equal(validTo) && f.CorrectedBy == correctedBy {
			return nil
		}
		return ErrValidityAlreadyClosed
	}
	vt := validTo
	f.ValidTo = &vt
	prev := f.CorrectedBy
	f.CorrectedBy = correctedBy
	if correctedBy != "" && prev != correctedBy {
		sh.correctedIdx[correctedBy] = append(sh.correctedIdx[correctedBy], factID)
	}
	return nil
}

// ReopenValidity clears the ValidTo / CorrectedBy fields on factID,
// guarded by expectedCorrectedBy. Used by Memory.Save's projection
// rollback to undo a supersede close after a downstream failure.
//
// The guard is essential: between the original close and the
// rollback another writer may legitimately have updated the same
// fact (different CorrectedBy). In that case ReopenValidity returns
// ErrReopenConflict and the caller must surface it via telemetry
// without touching the fact.
func (s *MemoryStore) ReopenValidity(_ context.Context, scope domain.Scope, factID string, expectedCorrectedBy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return ErrNotFound
	}
	f, ok := sh.byID[factID]
	if !ok {
		return ErrNotFound
	}
	// Already open — no-op; rollback can re-issue safely.
	if f.ValidTo == nil && f.CorrectedBy == "" {
		return nil
	}
	if f.CorrectedBy != expectedCorrectedBy {
		return ErrReopenConflict
	}
	prev := f.CorrectedBy
	f.ValidTo = nil
	f.CorrectedBy = ""
	if prev != "" {
		sh.correctedIdx[prev] = removeID(sh.correctedIdx[prev], factID)
		if len(sh.correctedIdx[prev]) == 0 {
			delete(sh.correctedIdx, prev)
		}
	}
	return nil
}

// UpdateFeedback adds deltas to Reinforcement / Penalty (Phase D.4).
func (s *MemoryStore) UpdateFeedback(_ context.Context, scope domain.Scope, factID string, reinforcementDelta, penaltyDelta float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return ErrNotFound
	}
	f, ok := sh.byID[factID]
	if !ok {
		return ErrNotFound
	}
	f.Reinforcement = clampNonNeg(f.Reinforcement + reinforcementDelta)
	f.Penalty = clampNonNeg(f.Penalty + penaltyDelta)
	return nil
}

func clampNonNeg(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// Delete removes facts by id. Missing ids are ignored.
func (s *MemoryStore) Delete(_ context.Context, scope domain.Scope, factIDs []string) error {
	if len(factIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.byScope[keyOf(scope)]
	if !ok {
		return nil
	}
	removed := make(map[string]struct{}, len(factIDs))
	for _, id := range factIDs {
		f, ok := sh.byID[id]
		if !ok {
			continue
		}
		delete(sh.byID, id)
		removed[id] = struct{}{}
		if f.MergeKey != "" {
			sh.mergeKeyIdx[f.MergeKey] = removeID(sh.mergeKeyIdx[f.MergeKey], id)
			if len(sh.mergeKeyIdx[f.MergeKey]) == 0 {
				delete(sh.mergeKeyIdx, f.MergeKey)
			}
		}
		if f.CorrectedBy != "" {
			sh.correctedIdx[f.CorrectedBy] = removeID(sh.correctedIdx[f.CorrectedBy], id)
			if len(sh.correctedIdx[f.CorrectedBy]) == 0 {
				delete(sh.correctedIdx, f.CorrectedBy)
			}
		}
	}
	if len(removed) > 0 {
		filtered := sh.orderedIDs[:0]
		for _, id := range sh.orderedIDs {
			if _, gone := removed[id]; gone {
				continue
			}
			filtered = append(filtered, id)
		}
		sh.orderedIDs = filtered
	}
	return nil
}

func (s *MemoryStore) Close() error { return nil }

// isSuperseded reports whether a fact has been replaced by another
// canonical write. Per docs §5.4 the only canonical signal for that
// is a non-empty CorrectedBy — ValidTo on its own is intrinsic time
// (e.g. an event's end timestamp) and must not hide the fact from
// the default List view.
func isSuperseded(f *domain.TemporalFact) bool {
	return f.CorrectedBy != ""
}

func hasAllEntities(f *domain.TemporalFact, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(f.Entities))
	for _, e := range f.Entities {
		have[e] = struct{}{}
	}
	for _, e := range required {
		if _, ok := have[e]; !ok {
			return false
		}
	}
	return true
}

func removeID(ids []string, target string) []string {
	out := ids[:0]
	for _, id := range ids {
		if id == target {
			continue
		}
		out = append(out, id)
	}
	return out
}

var _ port.TemporalStore = (*MemoryStore)(nil)
