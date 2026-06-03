package workspace

import (
	"context"
	"sort"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/memory/recall/store/internal/sqlstmt"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type temporalStore struct {
	b *Backend
}

func (s *temporalStore) Append(ctx context.Context, facts []domain.TemporalFact) error {
	if len(facts) == 0 {
		return nil
	}
	staged := make([]domain.TemporalFact, 0, len(facts))
	seen := make(map[string]struct{}, len(facts))
	for _, f := range facts {
		if f.ID == "" {
			return errdefs.Validationf("recall workspace temporal: fact id is required")
		}
		if !f.Kind.IsValid() {
			return errdefs.Validationf("recall workspace temporal: invalid fact kind %q for fact %q", f.Kind, f.ID)
		}
		if f.Scope.RuntimeID == "" {
			return errdefs.Validationf("recall workspace temporal: fact %q missing scope.runtime_id", f.ID)
		}
		k := factKey(f.Scope, f.ID)
		if _, ok := seen[k]; ok {
			return errdefs.Conflictf("recall workspace temporal: duplicate fact id %q within append batch", f.ID)
		}
		seen[k] = struct{}{}
		staged = append(staged, f.Clone())
	}

	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return err
	}
	for _, f := range staged {
		if factIndex(st.Facts, f.Scope, f.ID) >= 0 {
			return errdefs.Conflictf("recall workspace temporal: duplicate fact id %q in scope", f.ID)
		}
	}
	st.Facts = append(st.Facts, staged...)
	return s.b.save(ctx, st)
}

func (s *temporalStore) Get(ctx context.Context, scope domain.Scope, factID string) (domain.TemporalFact, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return domain.TemporalFact{}, err
	}
	idx := factIndex(st.Facts, scope, factID)
	if idx < 0 {
		return domain.TemporalFact{}, temporalstore.ErrNotFound
	}
	return st.Facts[idx].Clone(), nil
}

func (s *temporalStore) List(ctx context.Context, scope domain.Scope, query port.ListQuery) ([]domain.TemporalFact, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return nil, err
	}
	kindSet := make(map[domain.FactKind]struct{}, len(query.Kinds))
	for _, k := range query.Kinds {
		kindSet[k] = struct{}{}
	}
	requiredEntities := sqlstmt.UniqueNonEmpty(query.Entities)
	out := make([]domain.TemporalFact, 0)
	for _, f := range st.Facts {
		if !samePartition(f.Scope, scope) {
			continue
		}
		if !query.IncludeSuperseded && f.CorrectedBy != "" {
			continue
		}
		if len(kindSet) > 0 {
			if _, ok := kindSet[f.Kind]; !ok {
				continue
			}
		}
		if !hasAllEntities(f, requiredEntities) {
			continue
		}
		out = append(out, f.Clone())
	}
	sortFacts(out)
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (s *temporalStore) FindByMergeKey(ctx context.Context, scope domain.Scope, mergeKey string) ([]domain.TemporalFact, error) {
	if mergeKey == "" {
		return nil, nil
	}
	return s.findFacts(ctx, scope, func(f domain.TemporalFact) bool { return f.MergeKey == mergeKey })
}

func (s *temporalStore) FindSupersededBy(ctx context.Context, scope domain.Scope, factID string) ([]domain.TemporalFact, error) {
	if factID == "" {
		return nil, nil
	}
	return s.findFacts(ctx, scope, func(f domain.TemporalFact) bool { return f.CorrectedBy == factID })
}

func (s *temporalStore) FindByRevisionSource(ctx context.Context, scope domain.Scope, sourceFactID string) ([]domain.TemporalFact, error) {
	if sourceFactID == "" {
		return nil, nil
	}
	return s.findFacts(ctx, scope, func(f domain.TemporalFact) bool {
		rev, ok := domain.RevisionOf(f)
		return ok && rev.SourceFactID == sourceFactID
	})
}

func (s *temporalStore) FindByOriginRequestID(ctx context.Context, scope domain.Scope, requestID string) ([]domain.TemporalFact, error) {
	if requestID == "" {
		return nil, nil
	}
	return s.findFacts(ctx, scope, func(f domain.TemporalFact) bool { return f.Origin.RequestID == requestID })
}

func (s *temporalStore) UpdateValidity(ctx context.Context, scope domain.Scope, factID string, validTo time.Time, correctedBy string) error {
	return s.updateFact(ctx, scope, factID, func(f *domain.TemporalFact) error {
		if f.ValidTo != nil {
			if f.ValidTo.Equal(validTo) && f.CorrectedBy == correctedBy {
				return nil
			}
			return temporalstore.ErrValidityAlreadyClosed
		}
		vt := validTo
		f.ValidTo = &vt
		f.CorrectedBy = correctedBy
		return nil
	})
}

func (s *temporalStore) ReopenValidity(ctx context.Context, scope domain.Scope, factID string, expectedCorrectedBy string) error {
	return s.updateFact(ctx, scope, factID, func(f *domain.TemporalFact) error {
		if f.ValidTo == nil && f.CorrectedBy == "" {
			return nil
		}
		if f.CorrectedBy != expectedCorrectedBy {
			return temporalstore.ErrReopenConflict
		}
		f.ValidTo = nil
		f.CorrectedBy = ""
		return nil
	})
}

func (s *temporalStore) Delete(ctx context.Context, scope domain.Scope, factIDs []string) error {
	if len(factIDs) == 0 {
		return nil
	}
	targets := make(map[string]struct{}, len(factIDs))
	for _, id := range factIDs {
		if id != "" {
			targets[id] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return nil
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return err
	}
	filtered := st.Facts[:0]
	for _, f := range st.Facts {
		if samePartition(f.Scope, scope) {
			if _, ok := targets[f.ID]; ok {
				continue
			}
		}
		filtered = append(filtered, f)
	}
	st.Facts = filtered
	return s.b.save(ctx, st)
}

func (s *temporalStore) UpdateFeedback(ctx context.Context, scope domain.Scope, factID string, reinforcementDelta, penaltyDelta float64) error {
	return s.updateFact(ctx, scope, factID, func(f *domain.TemporalFact) error {
		f.Reinforcement = sqlstmt.ClampNonNeg(f.Reinforcement + reinforcementDelta)
		f.Penalty = sqlstmt.ClampNonNeg(f.Penalty + penaltyDelta)
		return nil
	})
}

func (s *temporalStore) MarkClosed(ctx context.Context, scope domain.Scope, factID string, closed bool) error {
	return s.updateFact(ctx, scope, factID, func(f *domain.TemporalFact) error {
		f.Closed = closed
		return nil
	})
}

func (s *temporalStore) ListByID(ctx context.Context, scope domain.Scope, factID string) ([]domain.TemporalFact, error) {
	seed, err := s.Get(ctx, scope, factID)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{seed.ID: {}}
	out := []domain.TemporalFact{seed}
	queue := []string{seed.ID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		f, err := s.Get(ctx, scope, id)
		if err != nil {
			continue
		}
		for _, prior := range f.Supersedes {
			if _, ok := seen[prior]; prior == "" || ok {
				continue
			}
			seen[prior] = struct{}{}
			pf, err := s.Get(ctx, scope, prior)
			if err == nil {
				out = append(out, pf)
				queue = append(queue, prior)
			}
		}
		successors, err := s.FindSupersededBy(ctx, scope, id)
		if err != nil {
			continue
		}
		for _, succ := range successors {
			if _, ok := seen[succ.ID]; ok {
				continue
			}
			seen[succ.ID] = struct{}{}
			out = append(out, succ)
			queue = append(queue, succ.ID)
		}
	}
	sortFacts(out)
	return out, nil
}

func (s *temporalStore) DeleteByScope(ctx context.Context, scope domain.Scope) (int, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	filtered := st.Facts[:0]
	for _, f := range st.Facts {
		if samePartition(f.Scope, scope) {
			n++
			continue
		}
		filtered = append(filtered, f)
	}
	st.Facts = filtered
	if err := s.b.save(ctx, st); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *temporalStore) ListScopes(ctx context.Context, query port.ScopeListQuery) ([]domain.Scope, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]domain.Scope{}
	for _, f := range st.Facts {
		if query.RuntimeID != "" && f.Scope.RuntimeID != query.RuntimeID {
			continue
		}
		scope := scopeFromPartition(f.Scope)
		seen[scope.PartitionKey()] = scope
	}
	scopes := make([]domain.Scope, 0, len(seen))
	for _, scope := range seen {
		scopes = append(scopes, scope)
	}
	sort.SliceStable(scopes, func(i, j int) bool {
		return scopes[i].PartitionKey() < scopes[j].PartitionKey()
	})
	return scopes, nil
}

func (s *temporalStore) Close() error { return nil }

func (s *temporalStore) ScopeGeneration(ctx context.Context, scope domain.Scope) (uint64, bool, error) {
	if scope.PartitionKey() == "" {
		return 0, false, errdefs.Validationf("recall workspace scope generation: scope partition is required")
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return 0, false, err
	}
	rec := st.ScopeGenerations[scope.PartitionKey()]
	return rec.Generation, rec.Deleting, nil
}

func (s *temporalStore) BumpScopeGeneration(ctx context.Context, scope domain.Scope, deleting bool) (uint64, error) {
	if scope.PartitionKey() == "" {
		return 0, errdefs.Validationf("recall workspace scope generation: scope partition is required")
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return 0, err
	}
	if st.ScopeGenerations == nil {
		st.ScopeGenerations = map[string]scopeGenerationRecord{}
	}
	key := scope.PartitionKey()
	rec := st.ScopeGenerations[key]
	rec.Generation++
	rec.Deleting = deleting
	rec.UpdatedAt = time.Now()
	st.ScopeGenerations[key] = rec
	return rec.Generation, s.b.save(ctx, st)
}

func (s *temporalStore) SetScopeDeleting(ctx context.Context, scope domain.Scope, deleting bool) error {
	if scope.PartitionKey() == "" {
		return errdefs.Validationf("recall workspace scope generation: scope partition is required")
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return err
	}
	if st.ScopeGenerations == nil {
		st.ScopeGenerations = map[string]scopeGenerationRecord{}
	}
	key := scope.PartitionKey()
	rec := st.ScopeGenerations[key]
	rec.Deleting = deleting
	rec.UpdatedAt = time.Now()
	st.ScopeGenerations[key] = rec
	return s.b.save(ctx, st)
}

func (s *temporalStore) findFacts(ctx context.Context, scope domain.Scope, match func(domain.TemporalFact) bool) ([]domain.TemporalFact, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.TemporalFact, 0)
	for _, f := range st.Facts {
		if samePartition(f.Scope, scope) && match(f) {
			out = append(out, f.Clone())
		}
	}
	sortFacts(out)
	return out, nil
}

func (s *temporalStore) updateFact(ctx context.Context, scope domain.Scope, factID string, mutate func(*domain.TemporalFact) error) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return err
	}
	idx := factIndex(st.Facts, scope, factID)
	if idx < 0 {
		return temporalstore.ErrNotFound
	}
	f := st.Facts[idx].Clone()
	if err := mutate(&f); err != nil {
		return err
	}
	st.Facts[idx] = f
	return s.b.save(ctx, st)
}

func factIndex(facts []domain.TemporalFact, scope domain.Scope, id string) int {
	for i, f := range facts {
		if samePartition(f.Scope, scope) && f.ID == id {
			return i
		}
	}
	return -1
}

func hasAllEntities(f domain.TemporalFact, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(f.Entities))
	for _, entity := range f.Entities {
		have[entity] = struct{}{}
	}
	for _, entity := range required {
		if _, ok := have[entity]; !ok {
			return false
		}
	}
	return true
}
