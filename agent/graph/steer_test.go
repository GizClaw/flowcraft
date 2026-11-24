package graph

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/errdefs"
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
	if _, err := run.queueSteer("s1", message.NewTextMessage(message.RoleUser, "steer"), agent.SteerRedirect); err != nil {
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
	if interrupted.Intent != agent.SteerRedirect {
		t.Errorf("intent = %q, want steer", interrupted.Intent)
	}
	if interrupted.Boundary != run.step+1 {
		t.Errorf("interruption = %+v, want boundary %d", interrupted, run.step+1)
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
	if _, err := run.queueSteer("s1", message.NewTextMessage(message.RoleUser, "steer"), agent.SteerRedirect); err == nil {
		t.Fatal("a graph with no input slot accepted a steer; it has nowhere to put it")
	}
}

// A steer is operator input, so it is a user turn. The alternative is an engine that can fabricate
// an assistant turn nobody wrote, which the model node then refuses a step later with an error that
// points at the transcript instead of at the caller.
func TestSteerRefusesANonUserMessage(t *testing.T) {
	g := build(t, steerDoc, &countKind{})
	run, err := g.Start(context.Background(), nil, WithSession("c1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, role := range []message.Role{message.RoleAssistant, message.RoleSystem, message.RoleTool} {
		if _, err := run.queueSteer("s1", message.NewTextMessage(role, "no"), agent.SteerRedirect); !errdefs.IsValidation(err) {
			t.Errorf("role %q: queueSteer = %v, want a validation error", role, err)
		}
	}
	if _, err := run.queueSteer("s1", message.NewTextMessage(message.RoleUser, "yes"), agent.SteerRedirect); err != nil {
		t.Fatalf("a user turn was refused: %v", err)
	}
}

// The slot has to declare a merge rule, because a steer is a writer the document may not have accounted
// for: under "single" (the default) a steer can collide with a node that writes the same key in the same
// step, which surfaces as a merge error the caller cannot attribute to its input. "append" and "last"
// both name a defined outcome — a transcript that grows, or a mailbox that keeps the newest input.
func TestSteerNeedsADeclaredMerge(t *testing.T) {
	cases := []struct {
		merge  string
		accept bool
	}{
		{merge: "", accept: false},
		{merge: "last", accept: true},
		{merge: "append", accept: true},
	}
	for _, c := range cases {
		declared := ""
		if c.merge != "" {
			declared = ", merge: " + c.merge
		}
		g := build(t, `
version: 1
entry: a
input: messages
state:
  messages: {type: messages`+declared+`}
nodes:
  - {id: a, kind: count}
edges:
  - {from: a, to: end}
`, &countKind{})
		run, err := g.Start(context.Background(), nil, WithSession("c1"))
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		_, err = run.queueSteer("s1", message.NewTextMessage(message.RoleUser, "steer"), agent.SteerRedirect)
		switch {
		case c.accept && err != nil:
			t.Errorf("merge %q: queueSteer = %v, want accepted", c.merge, err)
		case !c.accept && !errdefs.IsNotAvailable(err):
			t.Errorf("merge %q: queueSteer = %v, want not-available", c.merge, err)
		}
	}
}

// A steer queued during the last step never reaches a boundary, so the turn ends with an input nobody
// applied. The receipt is the only record of it: Steer's return value promised "queued" and nothing
// else, and a caller that waits for Interrupted would wait forever.
func TestSteerReportsAnInputThatNeverReachedABoundary(t *testing.T) {
	// One node, then end: there is no second frontier for the queued input to land on.
	g := build(t, `
version: 1
input: messages
entry: slow
state:
  messages: {type: messages, merge: append}
nodes:
  - {id: slow, kind: sleep, config: {d: 120ms}}
edges:
  - {from: slow, to: end}
`, &sleepKind{})
	program, drv := openDriver(t, g, "conv-1")
	defer func() { _ = program.Close() }()
	defer func() { _ = drv.Close() }()

	steerer := drv.(agent.Steerer)
	req := requestFor(agenttest.NewMemStore(), "task-1", "hi")
	events := make(chan []agent.Event, 1)
	go func() {
		var collected []agent.Event
		for event, err := range drv.Run(context.Background(), req) {
			if err != nil {
				t.Errorf("run: %v", err)
			}
			if event != nil {
				collected = append(collected, event)
			}
		}
		events <- collected
	}()
	waitForRun(t, drv)

	id, err := steerer.Steer(agent.SteerInput{
		Message: message.NewTextMessage(message.RoleUser, "too late"),
		Intent:  agent.SteerCorrect,
	})
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}

	var dropped *SteerDropped
	var interrupted bool
	for _, event := range <-events {
		switch e := event.(type) {
		case agent.Interrupted:
			interrupted = true
		case agent.EventExtension[SteerDropped]:
			value := e.Value
			dropped = &value
		}
	}
	if interrupted {
		t.Fatal("the input was applied although the run had no further boundary")
	}
	if dropped == nil {
		t.Fatal("the queued input was neither applied nor reported dropped")
	}
	if dropped.ID != id {
		t.Errorf("dropped id = %q, want the handle Steer returned (%q)", dropped.ID, id)
	}
	if dropped.Intent != agent.SteerCorrect {
		t.Errorf("dropped intent = %q, want %q", dropped.Intent, agent.SteerCorrect)
	}
}

// The other half of the receipt: an input that *is* applied reports Interrupted and nothing else. A run
// that reported both would leave a reader unable to tell whether its message landed.
func TestAppliedSteerReportsNoDrop(t *testing.T) {
	g := build(t, `
version: 1
input: messages
entry: slow
state:
  messages: {type: messages, merge: append}
nodes:
  - {id: slow, kind: sleep, config: {d: 120ms}}
  - {id: count, kind: count}
edges:
  - {from: slow, to: count}
  - {from: count, to: end}
`, &sleepKind{}, &countKind{})
	program, drv := openDriver(t, g, "conv-1")
	defer func() { _ = program.Close() }()
	defer func() { _ = drv.Close() }()

	req := requestFor(agenttest.NewMemStore(), "task-1", "hi")
	applied := make(chan bool, 1)
	go func() {
		var interrupted bool
		for event, err := range drv.Run(context.Background(), req) {
			if err != nil {
				t.Errorf("run: %v", err)
			}
			switch event.(type) {
			case agent.Interrupted:
				interrupted = true
			case agent.EventExtension[SteerDropped]:
				t.Errorf("an applied input was reported dropped")
			}
		}
		applied <- interrupted
	}()
	waitForRun(t, drv)
	if _, err := drv.(agent.Steerer).Steer(agent.SteerInput{
		Message: message.NewTextMessage(message.RoleUser, "hold on"),
		Intent:  agent.SteerRedirect,
	}); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if !<-applied {
		t.Fatal("the input reached a boundary but no Interrupted event reported it")
	}
}
