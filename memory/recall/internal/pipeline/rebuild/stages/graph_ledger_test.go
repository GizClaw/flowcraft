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

func TestGraphLedger_PreservesDocumentRawObservations(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	document := domain.Observation{
		ID:    "obs-doc",
		Scope: scope,
		Kind:  domain.ObservationKindDocument,
		Text:  "temperature = 0.2",
		Spans: []domain.ObservationSpan{{
			ID:            "span-doc",
			ObservationID: "obs-doc",
			Text:          "temperature = 0.2",
			Start:         0,
			End:           len("temperature = 0.2"),
		}},
	}
	observations := &rebuildObservationStore{listed: []domain.Observation{document}}
	stage := stages.NewGraphLedger(observations, &rebuildLinkStore{}, nil)
	if _, err := stage.Run(context.Background(), &rebuild.RebuildState{Scope: scope}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, observation := range observations.appended {
		if observation.ID == "obs-doc" && observation.Kind == domain.ObservationKindDocument {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("appended observations = %+v, want document raw observation preserved", observations.appended)
	}
}

func TestGraphLedger_RejectsDanglingParameterEvidence(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	observations := &rebuildObservationStore{}
	stage := stages.NewGraphLedger(observations, &rebuildLinkStore{}, nil)
	_, err := stage.Run(context.Background(), &rebuild.RebuildState{
		Scope: scope,
		Facts: []domain.TemporalFact{{
			ID:    "fact-param",
			Scope: scope,
			Kind:  domain.KindParameter,
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "missing-obs",
				SpanID:        "span-1",
				Text:          "mode = fast",
			}},
		}},
	})
	if err == nil {
		t.Fatal("Run err = nil, want dangling parameter evidence rejection")
	}
	if len(observations.appended) != 0 {
		t.Fatalf("observations appended despite preflight failure: %+v", observations.appended)
	}
}

func TestGraphLedger_RejectsParameterWithoutEvidenceRefs(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	observations := &rebuildObservationStore{}
	stage := stages.NewGraphLedger(observations, &rebuildLinkStore{}, nil)
	_, err := stage.Run(context.Background(), &rebuild.RebuildState{
		Scope: scope,
		Facts: []domain.TemporalFact{{
			ID:    "fact-param",
			Scope: scope,
			Kind:  domain.KindParameter,
		}},
	})
	if err == nil {
		t.Fatal("Run err = nil, want missing parameter evidence rejection")
	}
	if len(observations.appended) != 0 {
		t.Fatalf("observations appended despite preflight failure: %+v", observations.appended)
	}
}

func TestGraphLedger_RejectsUnsupportedParameterEvidenceText(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	observations := &rebuildObservationStore{listed: []domain.Observation{{
		ID:       "obs-1",
		Scope:    scope,
		Kind:     domain.ObservationKindTurn,
		SourceID: "turn-1",
		Text:     "favorite color = blue",
		Spans: []domain.ObservationSpan{{
			ID:            "span-1",
			ObservationID: "obs-1",
			SourceID:      "turn-1",
			Text:          "favorite color = blue",
			Start:         0,
			End:           len("favorite color = blue"),
		}},
	}}}
	stage := stages.NewGraphLedger(observations, &rebuildLinkStore{}, nil)
	_, err := stage.Run(context.Background(), &rebuild.RebuildState{
		Scope: scope,
		Facts: []domain.TemporalFact{{
			ID:     "fact-param",
			Scope:  scope,
			Kind:   domain.KindParameter,
			Object: "0.2",
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "obs-1",
				SpanID:        "span-1",
				Text:          "favorite color = blue",
			}},
			Metadata: map[string]any{
				domain.MetaParameterOwner:           "experiment",
				domain.MetaParameterCanonicalName:   "temperature",
				domain.MetaParameterNameSurface:     "temperature",
				domain.MetaParameterOperation:       "set",
				domain.MetaParameterValueKind:       "number",
				domain.MetaParameterRawValue:        "0.2",
				domain.MetaParameterNormalizedValue: "0.2",
			},
		}},
	})
	if err == nil {
		t.Fatal("Run err = nil, want unsupported parameter evidence rejection")
	}
	if len(observations.appended) != 0 {
		t.Fatalf("observations appended despite preflight failure: %+v", observations.appended)
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

func (s *rebuildObservationStore) List(_ context.Context, _ domain.Scope, query port.ObservationListQuery) ([]domain.Observation, error) {
	if len(query.Kinds) == 0 {
		return append([]domain.Observation(nil), s.listed...), nil
	}
	allowed := map[domain.ObservationKind]struct{}{}
	for _, kind := range query.Kinds {
		allowed[kind] = struct{}{}
	}
	var out []domain.Observation
	for _, observation := range s.listed {
		if _, ok := allowed[observation.Kind]; ok {
			out = append(out, observation)
		}
	}
	return out, nil
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
