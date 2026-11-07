package agent

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// hookProvider lets a test submit to the mailbox from inside a round, which is
// the only way to exercise steering and interruption honestly.
type hookProvider struct {
	steps  []inference.GenerateResponse
	seen   []inference.GenerateRequest
	onCall func(index int)
}

func (p *hookProvider) GenerateStream(_ context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	p.seen = append(p.seen, request)
	index := len(p.seen) - 1
	if p.onCall != nil {
		p.onCall(index)
	}
	if index >= len(p.steps) {
		return nil, errors.New("hook provider ran out of steps")
	}
	return &fakeStream{resp: p.steps[index]}, nil
}

func (p *hookProvider) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	stream, err := p.GenerateStream(ctx, request)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	for {
		if _, err := stream.Next(ctx); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return inference.GenerateResponse{}, err
		}
	}
	return stream.Result()
}

// An interrupt cancels the running round and leaves its message queued.
func TestInterruptCancelsAndQueues(t *testing.T) {
	var session *Session
	provider := &hookProvider{steps: []inference.GenerateResponse{textResponse("one")}}
	provider.onCall = func(int) {}
	session, _ = newTestSession(t, provider)
	session.markRunning(true)
	if _, err := session.Send(context.Background(),
		message.NewTextMessage(message.RoleUser, "stop"), ModeInterrupt); err != nil {
		t.Fatalf("Send interrupt: %v", err)
	}
	session.markRunning(false)
	// The message stays in the mailbox; the next Task consumes it.
	queued := session.takeQueued()
	if len(queued) != 1 || queued[0].Content.Text() != "stop" {
		t.Fatalf("queued = %d messages, want the interrupt text", len(queued))
	}
}

// Idle sessions cannot steer: the mode degrades and says so.
func TestSteerWhileIdleDegradesToQueue(t *testing.T) {
	session, _ := newTestSession(t, &scriptedProvider{})
	effective, err := session.Send(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi"), ModeSteer)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if effective != ModeQueue {
		t.Errorf("effective mode = %s, want queue", effective)
	}
	if session.Pending() != 1 {
		t.Errorf("pending = %d, want 1", session.Pending())
	}
}

// Leftovers at the end of a Task are handed up, not dropped.
func TestUndeliveredIsReported(t *testing.T) {
	var session *Session
	provider := &hookProvider{steps: []inference.GenerateResponse{textResponse("done")}}
	provider.onCall = func(int) {
		if session != nil {
			_, _ = session.Send(context.Background(),
				message.NewTextMessage(message.RoleUser, "later"), ModeQueue)
		}
	}
	session, _ = newTestSession(t, provider)

	var undelivered int
	for event, err := range session.Run(context.Background(), Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "hi"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if report, ok := event.(Undelivered); ok {
			undelivered = len(report.Messages)
		}
	}
	if undelivered != 1 {
		t.Errorf("undelivered = %d, want the queued message", undelivered)
	}
	if session.Pending() != 0 {
		t.Errorf("pending = %d after a finished Task, want 0", session.Pending())
	}
}
