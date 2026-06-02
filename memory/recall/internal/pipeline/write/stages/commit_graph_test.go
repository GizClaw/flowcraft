package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

type linkStoreRecorder struct {
	appendErr error
	appended  []domain.FactLink
	deleted   []string
}

func (s *linkStoreRecorder) Append(_ context.Context, links []domain.FactLink) error {
	if s.appendErr != nil {
		return s.appendErr
	}
	s.appended = append(s.appended, links...)
	return nil
}

func (s *linkStoreRecorder) Get(context.Context, domain.Scope, string) (domain.FactLink, error) {
	return domain.FactLink{}, port.ErrNotFound
}

func (s *linkStoreRecorder) List(context.Context, domain.Scope, port.LinkListQuery) ([]domain.FactLink, error) {
	return nil, nil
}

func (s *linkStoreRecorder) FindByNode(context.Context, domain.Scope, domain.GraphNodeRef) ([]domain.FactLink, error) {
	return nil, nil
}

func (s *linkStoreRecorder) FindByMergeKey(context.Context, domain.Scope, string) ([]domain.FactLink, error) {
	return nil, nil
}

func (s *linkStoreRecorder) Delete(_ context.Context, _ domain.Scope, ids []string) error {
	s.deleted = append(s.deleted, ids...)
	return nil
}

func (s *linkStoreRecorder) DeleteByNode(context.Context, domain.Scope, domain.GraphNodeRef) (int, error) {
	return 0, nil
}

func (s *linkStoreRecorder) DeleteByScope(context.Context, domain.Scope) (int, error) {
	return 0, nil
}

func (s *linkStoreRecorder) Close() error { return nil }

func TestCommitGraph_LinkFailureCleansObservationsAndProjection(t *testing.T) {
	boom := errors.New("links down")
	observations := &observationStoreRecorder{}
	projection := &observationProjectionRecorder{}
	links := &linkStoreRecorder{appendErr: boom}
	stage := stages.NewCommitGraph(observations, links, projection)
	state := &write.WriteState{
		Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"},
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
			ID:      "fact-1",
			Scope:   domain.Scope{RuntimeID: "rt", UserID: "u1"},
			Content: "Alice likes tea",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:        "ev-1",
				MessageID: "msg-1",
				Text:      "Alice likes tea",
			}},
		}}},
	}

	_, err := stage.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want %v", err, boom)
	}
	if len(observations.appended) != 1 {
		t.Fatalf("appended observations = %d, want 1", len(observations.appended))
	}
	id := observations.appended[0].ID
	if len(projection.forgotten) != 1 || projection.forgotten[0] != id {
		t.Fatalf("projection forgotten = %v, want %q", projection.forgotten, id)
	}
	if len(observations.deleted) != 1 || observations.deleted[0] != id {
		t.Fatalf("deleted observations = %v, want %q", observations.deleted, id)
	}
}

func TestCommitGraph_CompensateCleansProjection(t *testing.T) {
	observations := &observationStoreRecorder{}
	projection := &observationProjectionRecorder{}
	links := &linkStoreRecorder{}
	stage := stages.NewCommitGraph(observations, links, projection)
	state := &write.WriteState{
		Scope:               domain.Scope{RuntimeID: "rt", UserID: "u1"},
		GraphObservationIDs: []string{"obs-1"},
		GraphLinkIDs:        []string{"link-1"},
	}

	if err := stage.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if len(projection.forgotten) != 1 || projection.forgotten[0] != "obs-1" {
		t.Fatalf("projection forgotten = %v, want obs-1", projection.forgotten)
	}
	if len(links.deleted) != 1 || links.deleted[0] != "link-1" {
		t.Fatalf("deleted links = %v, want link-1", links.deleted)
	}
	if len(observations.deleted) != 1 || observations.deleted[0] != "obs-1" {
		t.Fatalf("deleted observations = %v, want obs-1", observations.deleted)
	}
}
