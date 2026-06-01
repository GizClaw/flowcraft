package workspace

import (
	"context"
	"sort"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type observationStore struct {
	b *Backend
}

func (s *observationStore) Append(ctx context.Context, observations []domain.Observation) error {
	if len(observations) == 0 {
		return nil
	}
	staged := make([]domain.Observation, 0, len(observations))
	seen := map[string]struct{}{}
	for _, obs := range observations {
		if obs.ID == "" {
			return errdefs.Validationf("recall workspace observation: observation id is required")
		}
		if obs.Scope.RuntimeID == "" {
			return errdefs.Validationf("recall workspace observation: observation %q missing scope.runtime_id", obs.ID)
		}
		k := factKey(obs.Scope, obs.ID)
		if _, ok := seen[k]; ok {
			idx := -1
			for i := range staged {
				if factKey(staged[i].Scope, staged[i].ID) == k {
					idx = i
					break
				}
			}
			if idx >= 0 {
				merged, _, conflict := domain.MergeObservation(staged[idx], obs)
				if conflict {
					return errdefs.Conflictf("recall workspace observation: duplicate observation id %q within append batch", obs.ID)
				}
				staged[idx] = merged
			}
			continue
		}
		seen[k] = struct{}{}
		staged = append(staged, obs.Clone())
	}

	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return err
	}
	for _, obs := range staged {
		idx := observationIndex(st.Observations, obs.Scope, obs.ID)
		if idx >= 0 {
			merged, changed, conflict := domain.MergeObservation(st.Observations[idx], obs)
			if conflict {
				return errdefs.Conflictf("recall workspace observation: duplicate observation id %q in scope", obs.ID)
			}
			if changed {
				st.Observations[idx] = merged
			}
			continue
		}
		st.Observations = append(st.Observations, obs.Clone())
	}
	return s.b.save(ctx, st)
}

func (s *observationStore) Get(ctx context.Context, scope domain.Scope, observationID string) (domain.Observation, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return domain.Observation{}, err
	}
	idx := observationIndex(st.Observations, scope, observationID)
	if idx < 0 {
		return domain.Observation{}, port.ErrNotFound
	}
	return st.Observations[idx].Clone(), nil
}

func (s *observationStore) List(ctx context.Context, scope domain.Scope, query port.ObservationListQuery) ([]domain.Observation, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return nil, err
	}
	kindSet := make(map[domain.ObservationKind]struct{}, len(query.Kinds))
	for _, kind := range query.Kinds {
		kindSet[kind] = struct{}{}
	}
	out := make([]domain.Observation, 0)
	for _, obs := range st.Observations {
		if !samePartition(obs.Scope, scope) {
			continue
		}
		if len(kindSet) > 0 {
			if _, ok := kindSet[obs.Kind]; !ok {
				continue
			}
		}
		if query.SourceID != "" && obs.SourceID != query.SourceID {
			continue
		}
		out = append(out, obs.Clone())
	}
	sortObservations(out)
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (s *observationStore) Delete(ctx context.Context, scope domain.Scope, observationIDs []string) error {
	targets := targetSet(observationIDs)
	if len(targets) == 0 {
		return nil
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return err
	}
	filtered := st.Observations[:0]
	for _, obs := range st.Observations {
		if samePartition(obs.Scope, scope) {
			if _, ok := targets[obs.ID]; ok {
				continue
			}
		}
		filtered = append(filtered, obs)
	}
	st.Observations = filtered
	return s.b.save(ctx, st)
}

func (s *observationStore) DeleteByScope(ctx context.Context, scope domain.Scope) (int, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return 0, err
	}
	filtered := st.Observations[:0]
	deleted := 0
	for _, obs := range st.Observations {
		if samePartition(obs.Scope, scope) {
			deleted++
			continue
		}
		filtered = append(filtered, obs)
	}
	st.Observations = filtered
	return deleted, s.b.save(ctx, st)
}

func (s *observationStore) Close() error { return s.b.Close() }

type linkStore struct {
	b *Backend
}

func (s *linkStore) Append(ctx context.Context, links []domain.FactLink) error {
	if len(links) == 0 {
		return nil
	}
	staged := make([]domain.FactLink, 0, len(links))
	seenIDs := map[string]struct{}{}
	seenMergeKeys := map[string]struct{}{}
	for _, link := range links {
		if err := validateLink(link, "workspace"); err != nil {
			return err
		}
		idKey := factKey(link.Scope, link.ID)
		if _, ok := seenIDs[idKey]; ok {
			continue
		}
		seenIDs[idKey] = struct{}{}
		if link.MergeKey != "" {
			mergeKey := factKey(link.Scope, link.MergeKey)
			if _, ok := seenMergeKeys[mergeKey]; ok {
				continue
			}
			seenMergeKeys[mergeKey] = struct{}{}
		}
		staged = append(staged, link.Clone())
	}

	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return err
	}
	for _, link := range staged {
		if link.MergeKey != "" && linkMergeKeyIndex(st.Links, link.Scope, link.MergeKey) >= 0 {
			continue
		}
		if linkIndex(st.Links, link.Scope, link.ID) >= 0 {
			continue
		}
		st.Links = append(st.Links, link.Clone())
	}
	return s.b.save(ctx, st)
}

func (s *linkStore) Get(ctx context.Context, scope domain.Scope, linkID string) (domain.FactLink, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return domain.FactLink{}, err
	}
	idx := linkIndex(st.Links, scope, linkID)
	if idx < 0 {
		return domain.FactLink{}, port.ErrNotFound
	}
	return st.Links[idx].Clone(), nil
}

func (s *linkStore) List(ctx context.Context, scope domain.Scope, query port.LinkListQuery) ([]domain.FactLink, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return nil, err
	}
	out := filterLinks(st.Links, scope, query)
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (s *linkStore) FindByNode(ctx context.Context, scope domain.Scope, node domain.GraphNodeRef) ([]domain.FactLink, error) {
	if node.Kind == "" || node.ID == "" {
		return nil, nil
	}
	links, err := s.List(ctx, scope, port.LinkListQuery{})
	if err != nil {
		return nil, err
	}
	out := make([]domain.FactLink, 0, len(links))
	for _, link := range links {
		if link.From == node || link.To == node {
			out = append(out, link)
		}
	}
	return out, nil
}

func (s *linkStore) FindByMergeKey(ctx context.Context, scope domain.Scope, mergeKey string) ([]domain.FactLink, error) {
	if mergeKey == "" {
		return nil, nil
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return nil, err
	}
	idx := linkMergeKeyIndex(st.Links, scope, mergeKey)
	if idx < 0 {
		return nil, nil
	}
	return []domain.FactLink{st.Links[idx].Clone()}, nil
}

func (s *linkStore) Delete(ctx context.Context, scope domain.Scope, linkIDs []string) error {
	targets := targetSet(linkIDs)
	if len(targets) == 0 {
		return nil
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return err
	}
	filtered := st.Links[:0]
	for _, link := range st.Links {
		if samePartition(link.Scope, scope) {
			if _, ok := targets[link.ID]; ok {
				continue
			}
		}
		filtered = append(filtered, link)
	}
	st.Links = filtered
	return s.b.save(ctx, st)
}

func (s *linkStore) DeleteByNode(ctx context.Context, scope domain.Scope, node domain.GraphNodeRef) (int, error) {
	if node.Kind == "" || node.ID == "" {
		return 0, nil
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return 0, err
	}
	filtered := st.Links[:0]
	deleted := 0
	for _, link := range st.Links {
		if samePartition(link.Scope, scope) && (link.From == node || link.To == node) {
			deleted++
			continue
		}
		filtered = append(filtered, link)
	}
	st.Links = filtered
	return deleted, s.b.save(ctx, st)
}

func (s *linkStore) DeleteByScope(ctx context.Context, scope domain.Scope) (int, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	st, err := s.b.load(ctx)
	if err != nil {
		return 0, err
	}
	filtered := st.Links[:0]
	deleted := 0
	for _, link := range st.Links {
		if samePartition(link.Scope, scope) {
			deleted++
			continue
		}
		filtered = append(filtered, link)
	}
	st.Links = filtered
	return deleted, s.b.save(ctx, st)
}

func (s *linkStore) Close() error { return s.b.Close() }

func observationIndex(observations []domain.Observation, scope domain.Scope, id string) int {
	for i, obs := range observations {
		if samePartition(obs.Scope, scope) && obs.ID == id {
			return i
		}
	}
	return -1
}

func linkIndex(links []domain.FactLink, scope domain.Scope, id string) int {
	for i, link := range links {
		if samePartition(link.Scope, scope) && link.ID == id {
			return i
		}
	}
	return -1
}

func linkMergeKeyIndex(links []domain.FactLink, scope domain.Scope, mergeKey string) int {
	for i, link := range links {
		if samePartition(link.Scope, scope) && link.MergeKey == mergeKey {
			return i
		}
	}
	return -1
}

func filterLinks(links []domain.FactLink, scope domain.Scope, query port.LinkListQuery) []domain.FactLink {
	typeSet := make(map[domain.FactLinkType]struct{}, len(query.Types))
	for _, typ := range query.Types {
		typeSet[typ] = struct{}{}
	}
	out := make([]domain.FactLink, 0)
	for _, link := range links {
		if !samePartition(link.Scope, scope) {
			continue
		}
		if len(typeSet) > 0 {
			if _, ok := typeSet[link.Type]; !ok {
				continue
			}
		}
		if !nodeRefMatches(query.From, link.From) || !nodeRefMatches(query.To, link.To) {
			continue
		}
		out = append(out, link.Clone())
	}
	sortLinks(out)
	return out
}

func sortObservations(observations []domain.Observation) {
	sort.SliceStable(observations, func(i, j int) bool {
		if observations[i].ObservedAt.Equal(observations[j].ObservedAt) {
			return observations[i].ID < observations[j].ID
		}
		return observations[i].ObservedAt.Before(observations[j].ObservedAt)
	})
}

func sortLinks(links []domain.FactLink) {
	sort.SliceStable(links, func(i, j int) bool {
		if links[i].CreatedAt.Equal(links[j].CreatedAt) {
			return links[i].ID < links[j].ID
		}
		return links[i].CreatedAt.Before(links[j].CreatedAt)
	})
}

func nodeRefMatches(query, actual domain.GraphNodeRef) bool {
	if query.Kind != "" && query.Kind != actual.Kind {
		return false
	}
	if query.ID != "" && query.ID != actual.ID {
		return false
	}
	return true
}

func targetSet(ids []string) map[string]struct{} {
	targets := map[string]struct{}{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		targets[id] = struct{}{}
	}
	return targets
}

func validateLink(link domain.FactLink, backend string) error {
	if link.ID == "" {
		return errdefs.Validationf("recall %s link: link id is required", backend)
	}
	if link.Scope.RuntimeID == "" {
		return errdefs.Validationf("recall %s link: link %q missing scope.runtime_id", backend, link.ID)
	}
	if link.Type == "" {
		return errdefs.Validationf("recall %s link: link %q missing type", backend, link.ID)
	}
	if link.From.Kind == "" || link.From.ID == "" || link.To.Kind == "" || link.To.ID == "" {
		return errdefs.Validationf("recall %s link: link %q requires from/to refs", backend, link.ID)
	}
	return nil
}
