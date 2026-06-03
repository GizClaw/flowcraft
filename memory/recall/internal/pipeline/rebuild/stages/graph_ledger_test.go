package stages_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/rebuild"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/rebuild/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestGraphLedger_RestoresPriorLinksWhenObservationClearFails(t *testing.T) {
	boom := errors.New("observations down")
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	priorLink := domain.FactLink{
		ID:        "link-1",
		Scope:     scope,
		Type:      domain.LinkSupports,
		From:      domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: "obs-1"},
		To:        domain.GraphNodeRef{Kind: domain.GraphNodeAssertion, ID: "fact-1"},
		CreatedAt: time.Unix(1, 0),
	}
	observations := &rebuildObservationStore{deleteByScopeErr: boom}
	links := &rebuildLinkStore{listed: []domain.FactLink{priorLink}}
	stage := stages.NewGraphLedger(observations, links, nil)

	_, err := stage.Run(context.Background(), &rebuild.RebuildState{Scope: scope})
	if err == nil {
		t.Fatal("Run must fail when observation clear fails")
	}
	if len(links.appended) != 1 || links.appended[0].ID != priorLink.ID {
		t.Fatalf("prior links not restored: %+v", links.appended)
	}
	if links.deleteByScopeCalls < 2 {
		t.Fatalf("expected original clear plus restore clear, got %d", links.deleteByScopeCalls)
	}
}

type rebuildObservationStore struct {
	listed           []domain.Observation
	appended         []domain.Observation
	deleteByScopeErr error
}

func (s *rebuildObservationStore) Append(_ context.Context, observations []domain.Observation) error {
	s.appended = append(s.appended, observations...)
	return nil
}

func (s *rebuildObservationStore) Get(context.Context, domain.Scope, string) (domain.Observation, error) {
	return domain.Observation{}, port.ErrNotFound
}

func (s *rebuildObservationStore) List(context.Context, domain.Scope, port.ObservationListQuery) ([]domain.Observation, error) {
	return append([]domain.Observation(nil), s.listed...), nil
}

func (s *rebuildObservationStore) Delete(context.Context, domain.Scope, []string) error {
	return nil
}

func (s *rebuildObservationStore) DeleteByScope(context.Context, domain.Scope) (int, error) {
	if s.deleteByScopeErr != nil {
		return 0, s.deleteByScopeErr
	}
	return len(s.listed), nil
}

func (s *rebuildObservationStore) Close() error { return nil }

type rebuildLinkStore struct {
	listed             []domain.FactLink
	appended           []domain.FactLink
	deleteByScopeCalls int
}

func (s *rebuildLinkStore) Append(_ context.Context, links []domain.FactLink) error {
	s.appended = append(s.appended, links...)
	return nil
}

func (s *rebuildLinkStore) Get(context.Context, domain.Scope, string) (domain.FactLink, error) {
	return domain.FactLink{}, port.ErrNotFound
}

func (s *rebuildLinkStore) List(context.Context, domain.Scope, port.LinkListQuery) ([]domain.FactLink, error) {
	return append([]domain.FactLink(nil), s.listed...), nil
}

func (s *rebuildLinkStore) FindByNode(context.Context, domain.Scope, domain.GraphNodeRef) ([]domain.FactLink, error) {
	return nil, nil
}

func (s *rebuildLinkStore) FindByMergeKey(context.Context, domain.Scope, string) ([]domain.FactLink, error) {
	return nil, nil
}

func (s *rebuildLinkStore) Delete(context.Context, domain.Scope, []string) error {
	return nil
}

func (s *rebuildLinkStore) DeleteByNode(context.Context, domain.Scope, domain.GraphNodeRef) (int, error) {
	return 0, nil
}

func (s *rebuildLinkStore) DeleteByScope(context.Context, domain.Scope) (int, error) {
	s.deleteByScopeCalls++
	return len(s.listed), nil
}

func (s *rebuildLinkStore) Close() error { return nil }
