package port_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestCloneAsyncSemanticJob_IsolatesMutableSlices(t *testing.T) {
	turns := []domain.TurnContext{{ID: "t1", Text: "hello"}}
	msgs := []domain.Message{{Role: "user", Text: "hi"}}
	anchor := []domain.TemporalFact{{
		ID:       "f1",
		Metadata: map[string]any{"k": "v"},
	}}
	job := port.AsyncSemanticJob{
		RequestID:           "req-1",
		EpisodeFactIDs:      []string{"epi-1"},
		TurnsSnapshot:       turns,
		RecentMessages:      msgs,
		ExistingFactsAnchor: anchor,
	}
	cloned := port.CloneAsyncSemanticJob(job)

	turns[0].Text = "mutated"
	msgs[0].Text = "mutated"
	anchor[0].Metadata["k"] = "mutated"
	job.EpisodeFactIDs[0] = "mutated"

	if cloned.TurnsSnapshot[0].Text != "hello" {
		t.Errorf("TurnsSnapshot = %q, want hello", cloned.TurnsSnapshot[0].Text)
	}
	if cloned.RecentMessages[0].Text != "hi" {
		t.Errorf("RecentMessages = %q, want hi", cloned.RecentMessages[0].Text)
	}
	if cloned.EpisodeFactIDs[0] != "epi-1" {
		t.Errorf("EpisodeFactIDs = %v, want epi-1", cloned.EpisodeFactIDs)
	}
	if cloned.ExistingFactsAnchor[0].Metadata["k"] != "v" {
		t.Errorf("anchor metadata = %v, want v", cloned.ExistingFactsAnchor[0].Metadata["k"])
	}
}

func TestCloneAsyncSemanticJob_PreservesScalarFields(t *testing.T) {
	at := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	job := port.AsyncSemanticJob{
		RequestID:  "req-2",
		Scope:      domain.Scope{RuntimeID: "rt", UserID: "u1"},
		ObservedAt: at,
		Tier:       "core",
		Attempt:    2,
	}
	cloned := port.CloneAsyncSemanticJob(job)
	if cloned.RequestID != job.RequestID || cloned.Tier != job.Tier || cloned.Attempt != 2 {
		t.Fatalf("cloned scalars = %+v, want %+v", cloned, job)
	}
	if !cloned.ObservedAt.Equal(at) {
		t.Errorf("ObservedAt = %v, want %v", cloned.ObservedAt, at)
	}
}
