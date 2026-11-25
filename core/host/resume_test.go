package host

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// suspendHost opens one session whose driver stops to ask after its first step, so a test can drive
// the paused state through the host the way a graph does through the engine.
func suspendHost(t *testing.T, question string) (*Agents, *protocolDriver) {
	t.Helper()
	driver := &protocolDriver{question: question}
	agents := hostFor(t, func() (agent.Program, error) { return &protocolProgram{driver: driver}, nil })
	return agents, driver
}

// protocolDriver is a driver with the pair: it stops on the first turn, resumes on the second.
type protocolDriver struct {
	question string

	mu      sync.Mutex
	paused  bool
	runs    int
	resumes int
}

// state reads the counters the delivery worker writes from its own goroutine.
func (d *protocolDriver) state() (runs, resumes int, paused bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runs, d.resumes, d.paused
}

func (d *protocolDriver) Run(ctx context.Context, _ agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		d.mu.Lock()
		if d.paused {
			d.mu.Unlock()
			yield(nil, agent.ErrAwaitingResume)
			return
		}
		d.runs++
		first := d.runs == 1
		if first {
			d.paused = true
		}
		d.mu.Unlock()
		if first {
			// The first turn stops to ask; a later one (the delivery) is an ordinary turn.
			d.paused = true
			yield(agent.Status{State: agent.StateInputRequired, Reason: d.question}, nil)
			return
		}
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (d *protocolDriver) Resume(context.Context, agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		d.mu.Lock()
		if !d.paused {
			d.mu.Unlock()
			yield(nil, agent.ErrAwaitingResume)
			return
		}
		d.resumes++
		d.paused = false
		d.mu.Unlock()
		yield(agent.Text{Text: "done"}, nil)
		yield(agent.Status{State: agent.StateCompleted}, nil)
	}
}

func (d *protocolDriver) Paused() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.paused
}

// AcceptsInput: a delivery may reach this conversation between turns.
func (d *protocolDriver) AcceptsInput() bool { return true }
func (d *protocolDriver) Close() error       { return nil }

type protocolProgram struct{ driver *protocolDriver }

func (p *protocolProgram) Open(agent.SessionInfo) (agent.Driver, error) { return p.driver, nil }
func (p *protocolProgram) Close() error                                 { return nil }

// TestARunIsRefusedWhileTheSessionWaits is the verb split at the host: a message offered to a paused
// conversation would be taken as the answer to a question the caller never saw.
func TestARunIsRefusedWhileTheSessionWaits(t *testing.T) {
	agents, driver := suspendHost(t, "which seat?")
	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	var conflict error
	for _, err := range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "another")) {
		conflict = err
	}
	if !errdefs.IsConflict(conflict) {
		t.Fatalf("Run = %v, want a conflict naming the pause", conflict)
	}
	if !strings.Contains(conflict.Error(), "Resume") {
		t.Fatalf("Run = %v, want it to name the verb that continues", conflict)
	}
	if runs, _, _ := driver.state(); runs != 1 {
		t.Fatalf("the driver ran %d times, want the refused message not to run", runs)
	}
}

// TestResumeContinuesThePausedSession is the other half: the answer goes through Resume, which the
// driver sees as a resume rather than as a new message.
func TestResumeContinuesThePausedSession(t *testing.T) {
	agents, driver := suspendHost(t, "which seat?")
	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}
	if _, _, paused := driver.state(); !paused {
		t.Fatal("the session is not waiting after a suspension")
	}
	var texts []string
	for event, err := range agents.Resume(t.Context(), "s1", message.NewTextContent("14A")) {
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if text, ok := event.(agent.Text); ok {
			texts = append(texts, text.Text)
		}
	}
	if runs, resumes, _ := driver.state(); resumes != 1 || runs != 1 {
		t.Fatalf("runs = %d, resumes = %d; want one of each", runs, resumes)
	}
	if len(texts) != 1 || texts[0] != "done" {
		t.Fatalf("texts = %v, want the resumed turn's output", texts)
	}
	if _, _, paused := driver.state(); paused {
		t.Fatal("the resume left the session paused")
	}
}

// TestResumeOfAnIdleSessionIsRefused keeps the verb honest: there is no question to answer, so the
// answer cannot quietly start a new task.
func TestResumeOfAnIdleSessionIsRefused(t *testing.T) {
	agents, _ := suspendHost(t, "which seat?")
	var conflict error
	for _, err := range agents.Resume(t.Context(), "s1", message.NewTextContent("14A")) {
		conflict = err
	}
	if !errdefs.IsConflict(conflict) {
		t.Fatalf("Resume = %v, want a conflict", conflict)
	}
}

// TestADeliveryWaitsOutThePause is the bug this split fixes: a delegated result offered while the
// parent is waiting for an answer is not taken as that answer, and is delivered once the parent
// resumes.
func TestADeliveryWaitsOutThePause(t *testing.T) {
	agents, driver := suspendHost(t, "may I?")

	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}

	agents.deliver(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "from a child"))
	// A resumed turn and the delivery turn both land here; the delivery must be the third run.
	for range agents.Resume(t.Context(), "s1", message.NewTextContent("yes")) {
	}
	waitFor(t, "the delivery turn", func() bool {
		runs, _, _ := driver.state()
		return runs == 2
	})
	if _, resumes, _ := driver.state(); resumes != 1 {
		t.Fatalf("resumes = %d, want the answer and not the delivery", resumes)
	}
}
