package a2a_test

import (
	"context"
	"encoding/json"
	"iter"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/agent-a2a/server"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"

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
}

func (s *scripted) recordContext(id string) {
	s.mu.Lock()
	s.contexts = append(s.contexts, id)
	s.mu.Unlock()
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

func (s *scripted) wasCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

// peer starts one A2A server over a scripted engine.
func peer(t *testing.T, events ...agent.Event) (*httptest.Server, *scripted) {
	t.Helper()
	engine := &scripted{state: agent.StateSubmitted, events: events}
	srv, err := server.NewServer(server.ServerConfig{
		Card: server.CardConfig{Name: "peer", Version: "1.0.0", URL: "https://peer.test"},
		Sessions: func(_ context.Context, exec *a2asrv.ExecutorContext) (agent.Runner, error) {
			engine.recordContext(exec.ContextID)
			return engine, nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, engine
}

func client(t *testing.T, peerURL, conversationID string) *a2a.Runner {
	t.Helper()
	runner, err := a2a.New(context.Background(), a2a.Config{
		Peer:           peerURL,
		ConversationID: conversationID,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return runner
}

// The remote agent satisfies the same contract a local engine does.
func TestRunnerSatisfiesTheContract(t *testing.T) {
	agenttest.Runner(t, func(t *testing.T) agent.Runner {
		ts, _ := peer(t,
			agent.Status{State: agent.StateWorking},
			agent.StreamStart{Round: 1},
			agent.Text{Text: "hi"},
			agent.Status{State: agent.StateCompleted},
		)
		return client(t, ts.URL, "conv-"+t.Name())
	})
}

// The peer's answer arrives as text even though the wire carries it as artifact
// chunks: the runner hands its caller core events, not protocol ones.
func TestRunCarriesTheRemoteAnswer(t *testing.T) {
	ts, _ := peer(t,
		agent.Status{State: agent.StateWorking},
		agent.StreamStart{Round: 1},
		agent.Text{Text: "hello "},
		agent.Text{Text: "world"},
		agent.Status{State: agent.StateCompleted},
	)
	runner := client(t, ts.URL, "conv-1")
	var answer string
	for event, err := range runner.Run(context.Background(), message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if text, ok := event.(agent.Text); ok {
			answer += text.Text
		}
	}
	if answer != "hello world" {
		t.Fatalf("answer = %q, want the peer's text", answer)
	}
}

// A produced artifact crosses both ways with its identity and locator intact.
func TestArtifactCrossesTheProtocol(t *testing.T) {
	ts, _ := peer(t,
		agent.Status{State: agent.StateWorking},
		agent.ArtifactWritten{Artifact: agent.Artifact{
			ID: "a1", Name: "report", MediaType: "text/plain", Blob: "sha256:1",
		}},
		agent.Status{State: agent.StateCompleted},
	)
	runner := client(t, ts.URL, "conv-1")
	var artifacts []agent.Artifact
	for event, err := range runner.Run(context.Background(), message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if written, ok := event.(agent.ArtifactWritten); ok {
			artifacts = append(artifacts, written.Artifact)
		}
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %d, want one", len(artifacts))
	}
	if got := artifacts[0]; got.ID != "a1" || got.Name != "report" || got.Blob != "sha256:1" {
		t.Errorf("artifact = %+v, want the peer's id, name and reference", got)
	}
}

// A cancel reaches the peer's engine, and a restart still knows the task.
// The transport's annotations cross the protocol: cited tasks, extensions, and
// each part's own metadata keyed by its position.
func TestMetadataCrossesTheProtocol(t *testing.T) {
	ts, engine := peer(t,
		agent.Status{State: agent.StateWorking},
		agent.Status{State: agent.StateCompleted},
	)
	runner := client(t, ts.URL, "conv-1")
	msg := message.NewTextMessage(message.RoleUser, "hi")
	msg.Metadata = &message.Metadata{
		ReferenceTasks: []string{"task-1"},
		Extensions:     []string{"https://example.test/ext"},
		Attributes: map[string]json.RawMessage{
			"note":   json.RawMessage(`{"k":1}`),
			"part.0": json.RawMessage(`{"alt":"x"}`),
		},
	}
	for _, err := range runner.Run(context.Background(), msg) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	engine.mu.Lock()
	received := append([]message.Message(nil), engine.received...)
	engine.mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("the peer received %d messages, want 1", len(received))
	}
	metadata := received[0].Metadata
	if metadata == nil {
		t.Fatal("the peer received no metadata")
	}
	if len(metadata.ReferenceTasks) != 1 || metadata.ReferenceTasks[0] != "task-1" {
		t.Errorf("reference tasks = %v", metadata.ReferenceTasks)
	}
	if len(metadata.Extensions) != 1 || metadata.Extensions[0] != "https://example.test/ext" {
		t.Errorf("extensions = %v", metadata.Extensions)
	}
	if !sameJSON(t, metadata.Attributes["note"], `{"k":1}`) {
		t.Errorf("note = %s", metadata.Attributes["note"])
	}
	if !sameJSON(t, metadata.Attributes["part.0"], `{"alt":"x"}`) {
		t.Errorf("part metadata = %s", metadata.Attributes["part.0"])
	}
}

// sameJSON compares two JSON documents structurally.
func sameJSON(t *testing.T, left json.RawMessage, right string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(left, &a); err != nil {
		t.Fatalf("decode %s: %v", left, err)
	}
	if err := json.Unmarshal([]byte(right), &b); err != nil {
		t.Fatalf("decode %s: %v", right, err)
	}
	return reflect.DeepEqual(a, b)
}

// The peer's context outlives the task: a restart continues the conversation even
// though the task it belonged to already finished.
func TestRestartKeepsTheRemoteContext(t *testing.T) {
	ts, engine := peer(t,
		agent.Status{State: agent.StateWorking},
		agent.Status{State: agent.StateCompleted},
	)
	store := agenttest.NewMemStore()
	ctx := context.Background()
	first, err := a2a.New(ctx, a2a.Config{Peer: ts.URL, ConversationID: "conv-1", Store: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range first.Run(ctx, message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	before := engine.lastContext()
	if before == "" {
		t.Fatal("the peer assigned no context")
	}
	restored, err := a2a.New(ctx, a2a.Config{Peer: ts.URL, ConversationID: "conv-1", Store: store})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	// The finished task is not resumed, but the conversation it belonged to is.
	if state := restored.State(); state != agent.StateSubmitted {
		t.Errorf("restored state = %s, want submitted for a finished task", state)
	}
	for _, err := range restored.Run(ctx, message.NewTextMessage(message.RoleUser, "again")) {
		if err != nil {
			t.Fatalf("Run after restart: %v", err)
		}
	}
	if after := engine.lastContext(); after != before {
		t.Errorf("context after restart = %q, want the peer's %q", after, before)
	}
}

func TestCancelReachesThePeerAndTheTaskSurvivesARestart(t *testing.T) {
	ts, engine := peer(t,
		agent.Status{State: agent.StateWorking},
		agent.Status{State: agent.StateInputRequired},
	)
	store := agenttest.NewMemStore()
	ctx := context.Background()
	runner, err := a2a.New(ctx, a2a.Config{Peer: ts.URL, ConversationID: "conv-1", Store: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range runner.Run(ctx, message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	if state := runner.State(); state != agent.StateInputRequired {
		t.Fatalf("state = %s, want input-required", state)
	}
	checkpoint, err := store.LatestForContext(ctx, "conv-1")
	if err != nil {
		t.Fatalf("LatestForContext: %v", err)
	}
	if checkpoint.Task.ID == "" || checkpoint.Task.State != agent.StateInputRequired {
		t.Fatalf("checkpoint = %s/%s, want the paused remote task",
			checkpoint.Task.ID, checkpoint.Task.State)
	}
	restored, err := a2a.New(ctx, a2a.Config{Peer: ts.URL, ConversationID: "conv-1", Store: store})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if state := restored.State(); state != agent.StateInputRequired {
		t.Errorf("restored state = %s, want input-required", state)
	}
	if err := restored.Cancel(ctx, "not needed"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !engine.wasCancelled() {
		t.Error("the cancel never reached the peer")
	}
}
