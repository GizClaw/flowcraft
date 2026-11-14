package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

const steerDoc = `
version: 1
entry: a
input: messages
state:
  messages: {type: messages, merge: append}
nodes:
  - {id: a, kind: count}
edges:
  - {from: a, to: end}
`

// A steer is queued and applied at the next boundary, not inside the current
// node: the appended message is visible to the next step.
func TestSteerQueuesToTheNextBoundary(t *testing.T) {
	g := build(t, steerDoc, &countKind{})
	run, err := g.Start(context.Background(), nil, WithSession("c1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := run.queueSteer(message.NewTextMessage(message.RoleUser, "steer"), agent.InterruptSteer); err != nil {
		t.Fatalf("queueSteer: %v", err)
	}
	var events []agent.Event
	ok, err := run.applySteer(func(event agent.Event, err error) bool {
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		events = append(events, event)
		return true
	})
	if err != nil || !ok {
		t.Fatalf("applySteer = %v, %v", ok, err)
	}
	transcript, err := run.values.values[g.input].Messages()
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if transcript.Len() != 1 {
		t.Fatalf("messages = %d, want the steered turn", transcript.Len())
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want one Interrupted", len(events))
	}
	interrupted, ok := events[0].(agent.Interrupted)
	if !ok {
		t.Fatalf("event = %T, want Interrupted", events[0])
	}
	if interrupted.IntentKind != agent.InterruptSteer {
		t.Errorf("kind = %q, want steer", interrupted.IntentKind)
	}
	if interrupted.BoundaryStep != run.step+1 || interrupted.BeforePlanHash == "" {
		t.Errorf("interruption = %+v, want the boundary and plan hash", interrupted)
	}
}

// A graph with no input slot has nowhere to put a steer; the capability is
// refused instead of dropping the message.
func TestSteerNeedsAnInputSlot(t *testing.T) {
	g := build(t, `
version: 1
entry: a
state:
  flag: {type: bool}
nodes:
  - {id: a, kind: count}
edges:
  - {from: a, to: end}
`, &countKind{})
	run, err := g.Start(context.Background(), nil, WithSession("c1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := run.queueSteer(message.NewTextMessage(message.RoleUser, "steer"), agent.InterruptSteer); !errors.Is(err, agent.ErrSteerUnsupported) {
		t.Fatalf("queueSteer = %v, want ErrSteerUnsupported", err)
	}
}
