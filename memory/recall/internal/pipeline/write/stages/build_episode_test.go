package stages_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
)

func TestBuildEpisode_HappyPathPopulatesStateAndDetail(t *testing.T) {
	s := stages.NewBuildEpisode()
	turnTime := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	state := &write.WriteState{
		Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"},
		Turns: []domain.TurnContext{
			{ID: "t1", EvidenceID: "ev1", Role: "user", Speaker: "Alice", Time: turnTime, Text: "hello"},
			{ID: "t2", EvidenceID: "ev2", Role: "assistant", Speaker: "Bot", Time: turnTime.Add(time.Second), Text: "world"},
		},
	}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.AsyncRequestID == "" {
		t.Fatal("AsyncRequestID must be stamped")
	}
	if len(state.EpisodeFacts) != 1 {
		t.Fatalf("EpisodeFacts len = %d, want 1", len(state.EpisodeFacts))
	}
	f := state.EpisodeFacts[0]
	if f.Kind != domain.KindEpisode {
		t.Errorf("Kind = %q, want episode", f.Kind)
	}
	if f.Origin.Kind != domain.OriginKindEpisode || f.Origin.RequestID != state.AsyncRequestID {
		t.Errorf("Origin = %+v", f.Origin)
	}
	if !strings.Contains(f.Content, "Alice: hello") || !strings.Contains(f.Content, "Bot: world") {
		t.Errorf("Content = %q", f.Content)
	}
	if len(f.EvidenceRefs) != 2 {
		t.Errorf("EvidenceRefs len = %d, want 2", len(f.EvidenceRefs))
	}
	if len(f.SourceMessageIDs) != 2 || f.SourceMessageIDs[0] != "t1" {
		t.Errorf("SourceMessageIDs = %v", f.SourceMessageIDs)
	}
	if f.ValidFrom == nil || !f.ValidFrom.Equal(f.ObservedAt) {
		t.Errorf("ValidFrom != ObservedAt: %v vs %v", f.ValidFrom, f.ObservedAt)
	}
	detail, ok := d.(diagnostic.BuildEpisodeDetail)
	if !ok {
		t.Fatalf("Detail type = %T", d)
	}
	if detail.AsyncRequestID != state.AsyncRequestID || detail.EpisodeFacts != 1 || detail.Turns != 2 {
		t.Errorf("Detail = %+v", detail)
	}
}

func TestBuildEpisode_UsesObservedAtFromRequestWhenSet(t *testing.T) {
	s := stages.NewBuildEpisode()
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state := &write.WriteState{
		Scope:      domain.Scope{RuntimeID: "rt"},
		ObservedAt: anchor,
		Turns: []domain.TurnContext{
			{ID: "t1", Speaker: "Alice", Text: "hi"},
		},
	}
	if _, err := s.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	f := state.EpisodeFacts[0]
	if !f.ObservedAt.Equal(anchor) {
		t.Errorf("ObservedAt = %v, want %v", f.ObservedAt, anchor)
	}
}

func TestBuildEpisode_NoEvidenceIDLeavesRefsEmpty(t *testing.T) {
	s := stages.NewBuildEpisode()
	state := &write.WriteState{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []domain.TurnContext{{ID: "t1", Speaker: "Alice", Text: "hi"}},
	}
	if _, err := s.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(state.EpisodeFacts[0].EvidenceRefs) != 0 {
		t.Errorf("EvidenceRefs must be empty when no turn carries EvidenceID")
	}
}
