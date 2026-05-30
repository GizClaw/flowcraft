package stages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
)

func TestAppendEpisode_HappyPathRecordsIDs(t *testing.T) {
	store := &fakeStore{}
	s := stages.NewAppendEpisode(store, nil)
	state := &write.WriteState{
		Scope: domain.Scope{RuntimeID: "rt"},
		EpisodeFacts: []domain.TemporalFact{
			{ID: "epi-1", Kind: domain.KindEpisode},
		},
	}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := d.(diagnostic.AppendDetail).Facts; got != 1 {
		t.Errorf("Detail.Facts = %d, want 1", got)
	}
	if len(state.EpisodeFacts) != 1 || state.EpisodeFacts[0].ID != "epi-1" {
		t.Errorf("EpisodeFacts = %+v", state.EpisodeFacts)
	}
	if len(store.appended) != 1 {
		t.Errorf("store.appended = %d, want 1", len(store.appended))
	}
}

func TestAppendEpisode_StoreErrorPropagates(t *testing.T) {
	boom := errors.New("store down")
	s := stages.NewAppendEpisode(&fakeStore{appendErr: boom}, nil)
	state := &write.WriteState{
		Scope:        domain.Scope{RuntimeID: "rt"},
		EpisodeFacts: []domain.TemporalFact{{ID: "epi-1"}},
	}
	_, err := s.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if state.FailedStage != "append_episode" {
		t.Errorf("FailedStage = %q", state.FailedStage)
	}
}

func TestAppendEpisode_CompensateDeletesEpisodeFacts(t *testing.T) {
	store := &fakeStore{}
	s := stages.NewAppendEpisode(store, nil)
	state := &write.WriteState{
		Scope: domain.Scope{RuntimeID: "rt"},
		EpisodeFacts: []domain.TemporalFact{
			{ID: "epi-1"}, {ID: "epi-2"},
		},
		AppendedFactIDs: []string{"epi-1", "epi-2"},
	}
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if len(store.deleted) != 2 || store.deleted[0] != "epi-1" {
		t.Errorf("deleted = %v", store.deleted)
	}
}

func TestAppendEpisode_CompensateEmitsTelemetryOnDeleteFailure(t *testing.T) {
	boom := errors.New("delete unavailable")
	store := &fakeStore{deleteErr: boom}
	hook := &recordHook{}
	s := stages.NewAppendEpisode(store, hook)
	state := &write.WriteState{
		Scope:        domain.Scope{RuntimeID: "rt"},
		EpisodeFacts: []domain.TemporalFact{{ID: "epi-1"}},
		FailedStage:  "write_semantic_outbox",
	}
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate (best-effort) must not return err: %v", err)
	}
	if len(hook.events) != 1 {
		t.Fatalf("hook events = %d, want 1", len(hook.events))
	}
	ev := hook.events[0]
	if ev.Status != diagnostic.StatusFailed {
		t.Errorf("Status = %q, want failed", ev.Status)
	}
	d, ok := ev.Detail.(diagnostic.CompensationFailedDetail)
	if !ok {
		t.Fatalf("Detail type = %T", ev.Detail)
	}
	if d.OriginalStage != "save_rollback.episode_delete" {
		t.Errorf("OriginalStage = %q", d.OriginalStage)
	}
	if d.Cause != "write_semantic_outbox" {
		t.Errorf("Cause = %q", d.Cause)
	}
}
