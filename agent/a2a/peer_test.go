package a2a_test

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/agent/a2a/wire"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// scripted is the peer's engine: it reports a fixed sequence and remembers a
// cancellation.
type scripted struct {
	mu        sync.Mutex
	state     agent.TaskState
	events    []agent.Event
	cancelled bool
	received  []message.Message
	contexts  []string
	// taskID is what the peer named the task it ran, recorded from the executor's own info.
	taskID string
}

func (s *scripted) recordTask(id string) {
	s.mu.Lock()
	s.taskID = id
	s.mu.Unlock()
}

func (s *scripted) recordContext(id string) {
	s.mu.Lock()
	s.contexts = append(s.contexts, id)
	s.mu.Unlock()
}

// lastTaskID is the peer's id for the task its engine last ran, which is what a resumed turn
// must name.
func (s *scripted) lastTaskID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.taskID
}

// wasCancelled reports whether the peer's engine was asked to stop.
func (s *scripted) wasCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

func (s *scripted) lastContext() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.contexts) == 0 {
		return ""
	}
	return s.contexts[len(s.contexts)-1]
}

func (s *scripted) State() agent.TaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *scripted) Run(_ context.Context, msg message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		s.mu.Lock()
		s.state = agent.StateWorking
		s.received = append(s.received, msg)
		events := append([]agent.Event(nil), s.events...)
		s.mu.Unlock()
		for _, event := range events {
			if status, ok := event.(agent.Status); ok {
				s.mu.Lock()
				s.state = status.State
				s.mu.Unlock()
			}
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (s *scripted) Cancel(context.Context, string) error {
	s.mu.Lock()
	s.cancelled = true
	s.state = agent.StateCanceled
	s.mu.Unlock()
	return nil
}

func (s *scripted) Close() error { return nil }

// peerExecutor exposes a scripted engine over A2A. It is deliberately minimal:
// the client tests care about the protocol, not about a deployment's server.
type peerExecutor struct{ engine *scripted }

func (e *peerExecutor) Execute(ctx context.Context, exec *a2asrv.ExecutorContext) iter.Seq2[sdk.Event, error] {
	return func(yield func(sdk.Event, error) bool) {
		e.engine.recordContext(exec.ContextID)
		info := exec.TaskInfo()
		e.engine.recordTask(string(info.TaskID))
		if exec.StoredTask == nil {
			if !yield(sdk.NewSubmittedTask(info, exec.Message), nil) {
				return
			}
		}
		turn, err := wire.FromMessage(exec.Message)
		if err != nil {
			yield(nil, err)
			return
		}
		for event, runErr := range e.engine.Run(ctx, turn) {
			if runErr != nil {
				yield(nil, runErr)
				return
			}
			for _, converted := range peerEvents(info, event) {
				if !yield(converted, nil) {
					return
				}
			}
		}
	}
}

func (e *peerExecutor) Cancel(ctx context.Context, exec *a2asrv.ExecutorContext) iter.Seq2[sdk.Event, error] {
	return func(yield func(sdk.Event, error) bool) {
		if err := e.engine.Cancel(ctx, "cancelled"); err != nil {
			yield(nil, err)
			return
		}
		yield(sdk.NewStatusUpdateEvent(exec.TaskInfo(), sdk.TaskStateCanceled, nil), nil)
	}
}

func peerEvents(info sdk.TaskInfo, event agent.Event) []sdk.Event {
	switch typed := event.(type) {
	case agent.Status:
		var msg *sdk.Message
		if typed.Reason != "" {
			msg = &sdk.Message{
				ID:    sdk.NewMessageID(),
				Role:  sdk.MessageRoleAgent,
				Parts: sdk.ContentParts{sdk.NewTextPart(typed.Reason)},
			}
		}
		return []sdk.Event{sdk.NewStatusUpdateEvent(info, wire.State(typed.State), msg)}
	case agent.Text:
		if typed.Text == "" {
			return nil
		}
		return []sdk.Event{&sdk.TaskArtifactUpdateEvent{
			TaskID:    info.TaskID,
			ContextID: info.ContextID,
			LastChunk: true,
			Artifact: &sdk.Artifact{
				ID:    sdk.NewArtifactID(),
				Parts: sdk.ContentParts{sdk.NewTextPart(typed.Text)},
			},
		}}
	case agent.ArtifactWritten:
		return []sdk.Event{&sdk.TaskArtifactUpdateEvent{
			TaskID:    info.TaskID,
			ContextID: info.ContextID,
			LastChunk: true,
			Artifact: &sdk.Artifact{
				ID:   sdk.ArtifactID(typed.Artifact.ID),
				Name: typed.Artifact.Name,
				Parts: sdk.ContentParts{sdk.NewDataPart(map[string]string{
					"blob":       typed.Artifact.Blob,
					"media_type": typed.Artifact.MediaType,
					"name":       typed.Artifact.Name,
				})},
			},
		}}
	}
	return nil
}

// peer starts one A2A server over a scripted engine.
func peer(t *testing.T, events ...agent.Event) (*httptest.Server, *scripted) {
	t.Helper()
	engine := &scripted{state: agent.StateSubmitted, events: events}
	card := &sdk.AgentCard{
		Name:    "peer",
		Version: "1.0.0",
		SupportedInterfaces: []*sdk.AgentInterface{
			sdk.NewAgentInterface("https://peer.test", sdk.TransportProtocolJSONRPC),
		},
	}
	handler := a2asrv.NewHandler(&peerExecutor{engine: engine})
	mux := http.NewServeMux()
	mux.Handle("/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(handler))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, engine
}
