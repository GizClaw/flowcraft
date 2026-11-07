package manage

import (
	"context"
	"iter"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// cancelSession is a conversation that remembers being cancelled remotely.
type cancelSession struct {
	mu        sync.Mutex
	cancelled int
}

func (s *cancelSession) SetIdentity(string, string) {}
func (s *cancelSession) State() agent.TaskState     { return agent.StateWorking }
func (s *cancelSession) Run(context.Context, agent.Input) iter.Seq2[agent.Event, error] {
	return func(func(agent.Event, error) bool) {}
}
func (s *cancelSession) Resume(context.Context, agent.Reply) iter.Seq2[agent.Event, error] {
	return func(func(agent.Event, error) bool) {}
}
func (s *cancelSession) Send(context.Context, message.Message, agent.Mode) (agent.Mode, error) {
	return agent.ModeQueue, nil
}
func (s *cancelSession) Pending() int   { return 0 }
func (s *cancelSession) Abandon() error { return nil }
func (s *cancelSession) CancelRemote(context.Context) {
	s.mu.Lock()
	s.cancelled++
	s.mu.Unlock()
}

// A proxied conversation forwards the client's cancel; a local one has nothing
// to forward.
func TestCancelRemoteReachesThePeer(t *testing.T) {
	session := &cancelSession{}
	manager := New(WithEngines(Engine{
		Name: "fake",
		Build: func(context.Context, EngineSpec, *Agent) (engine, error) {
			return fakeEngine{session: session}, nil
		},
	}))
	t.Cleanup(func() { _ = manager.Close() })
	deployed, err := manager.Add(context.Background(), Spec{
		Name:           "a",
		Engine:         EngineSpec{Kind: "fake"},
		Runtime:        config(t),
		RuntimeOptions: []runtime.Option{driver(&scripted{steps: []inference.GenerateResponse{textStep("hi")}})},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// The conversation exists, with the fake session behind it. protocolRunner is
	// not used here: it registers a run and would hold the agent's wait group
	// open for a sequence nobody drains.
	deployed.mu.Lock()
	deployed.sessions["conv-1"] = &conversation{session: session}
	deployed.mu.Unlock()

	deployed.cancelRemote("conv-1")
	session.mu.Lock()
	cancelled := session.cancelled
	session.mu.Unlock()
	if cancelled != 1 {
		t.Errorf("cancelled = %d, want the peer asked once", cancelled)
	}
}

// fakeEngine opens the same session for every conversation.
type fakeEngine struct{ session session }

func (e fakeEngine) Open(context.Context, string) (session, error) { return e.session, nil }
func (fakeEngine) Close() error                                    { return nil }
