package stages_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

type fakeQueue struct {
	enqueued []port.AsyncSemanticJob
	err      error
}

func (q *fakeQueue) Enqueue(_ context.Context, job port.AsyncSemanticJob) (port.AsyncSemanticReceipt, error) {
	if q.err != nil {
		return port.AsyncSemanticReceipt{}, q.err
	}
	q.enqueued = append(q.enqueued, job)
	return port.AsyncSemanticReceipt{RequestID: job.RequestID, EnqueuedAt: time.Now()}, nil
}
func (q *fakeQueue) Cancel(_ context.Context, requestID string) error {
	for i := range q.enqueued {
		if q.enqueued[i].RequestID == requestID {
			q.enqueued = append(q.enqueued[:i], q.enqueued[i+1:]...)
			return nil
		}
	}
	return nil
}
func (q *fakeQueue) CancelScope(context.Context, domain.Scope) (int, error) { return 0, nil }
func (q *fakeQueue) CancelMatchingEpisodes(context.Context, domain.Scope, []string) (int, error) {
	return 0, nil
}
func (q *fakeQueue) Claim(context.Context, port.AsyncSemanticClaimOptions) ([]port.AsyncSemanticJob, error) {
	return nil, nil
}
func (q *fakeQueue) Complete(context.Context, string, port.AsyncSemanticResult) error { return nil }
func (q *fakeQueue) Fail(context.Context, string, port.AsyncSemanticFailure) error    { return nil }
func (q *fakeQueue) Stats(context.Context, port.AsyncSemanticStatsFilter) (port.AsyncSemanticStats, error) {
	return port.AsyncSemanticStats{}, nil
}

func TestWriteSemanticOutbox_HappyPathEnqueuesAndFlipsPending(t *testing.T) {
	q := &fakeQueue{}
	s := stages.NewWriteSemanticOutbox(q, nil)
	state := &write.WriteState{
		Scope:          domain.Scope{RuntimeID: "rt", UserID: "u1"},
		AsyncRequestID: "areq-1",
		EpisodeFacts:   []domain.TemporalFact{{ID: "epi-1"}, {ID: "epi-2"}},
		Turns:          []domain.TurnContext{{ID: "t1", Text: "hi"}},
		Tier:           "core",
	}
	d, err := s.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !state.SemanticPending {
		t.Errorf("SemanticPending = false")
	}
	if len(q.enqueued) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(q.enqueued))
	}
	job := q.enqueued[0]
	if job.RequestID != "areq-1" {
		t.Errorf("RequestID = %q", job.RequestID)
	}
	if len(job.EpisodeFactIDs) != 2 || job.EpisodeFactIDs[0] != "epi-1" {
		t.Errorf("EpisodeFactIDs = %v", job.EpisodeFactIDs)
	}
	if job.Tier != "core" {
		t.Errorf("Tier = %q", job.Tier)
	}
	detail, ok := d.(diagnostic.EnqueueSemanticDetail)
	if !ok {
		t.Fatalf("Detail type = %T", d)
	}
	if detail.AsyncRequestID != "areq-1" {
		t.Errorf("Detail.AsyncRequestID = %q", detail.AsyncRequestID)
	}
}

func TestWriteSemanticOutbox_EnqueueClonesCallerSlices(t *testing.T) {
	q := &fakeQueue{}
	s := stages.NewWriteSemanticOutbox(q, nil)
	turns := []domain.TurnContext{{ID: "t1", Text: "hello"}}
	state := &write.WriteState{
		Scope:          domain.Scope{RuntimeID: "rt"},
		AsyncRequestID: "areq-1",
		EpisodeFacts:   []domain.TemporalFact{{ID: "epi-1"}},
		Turns:          turns,
	}
	if _, err := s.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	turns[0].Text = "mutated"
	if len(q.enqueued) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(q.enqueued))
	}
	if q.enqueued[0].TurnsSnapshot[0].Text != "hello" {
		t.Errorf("queued TurnsSnapshot = %q, want hello", q.enqueued[0].TurnsSnapshot[0].Text)
	}
}

func TestWriteSemanticOutbox_EnqueueErrPropagates(t *testing.T) {
	boom := errors.New("outbox unavailable")
	s := stages.NewWriteSemanticOutbox(&fakeQueue{err: boom}, nil)
	state := &write.WriteState{
		Scope:          domain.Scope{RuntimeID: "rt"},
		AsyncRequestID: "areq-1",
		EpisodeFacts:   []domain.TemporalFact{{ID: "epi-1"}},
	}
	_, err := s.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if state.FailedStage != "write_semantic_outbox" {
		t.Errorf("FailedStage = %q", state.FailedStage)
	}
	if state.SemanticPending {
		t.Errorf("SemanticPending must stay false on failure")
	}
}

func TestWriteSemanticOutbox_NilQueueReturnsValidationLikeErr(t *testing.T) {
	s := stages.NewWriteSemanticOutbox(nil, nil)
	state := &write.WriteState{
		Scope:          domain.Scope{RuntimeID: "rt"},
		AsyncRequestID: "areq-1",
		EpisodeFacts:   []domain.TemporalFact{{ID: "epi-1"}},
	}
	_, err := s.Run(context.Background(), state)
	if err == nil {
		t.Fatal("nil queue must return error")
	}
	if state.FailedStage != "write_semantic_outbox" {
		t.Errorf("FailedStage = %q", state.FailedStage)
	}
}

func TestWriteSemanticOutbox_CompensateCancelsEnqueuedJob(t *testing.T) {
	q := &fakeQueue{}
	s := stages.NewWriteSemanticOutbox(q, nil)
	state := &write.WriteState{
		Scope:           domain.Scope{RuntimeID: "rt"},
		AsyncRequestID:  "areq-1",
		EpisodeFacts:    []domain.TemporalFact{{ID: "epi-1"}},
		SemanticPending: true,
	}
	q.enqueued = append(q.enqueued, port.AsyncSemanticJob{RequestID: "areq-1"})
	if err := s.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if len(q.enqueued) != 0 {
		t.Fatalf("Compensate must cancel outbox job, still have %d", len(q.enqueued))
	}
	if state.SemanticPending {
		t.Error("SemanticPending must clear after cancel")
	}
}
