package a2a_test

// Cross-adapter round trips. The fixtures in protocol_test.go check each side
// against a stub; these check the two sides against each other, through two real
// HTTP hops:
//
//	client (agent-a2a)                 what a remote caller sees
//	  -> server (this package)         the deployment's exposure
//	    -> engine (agent-a2a again)    the proxied peer
//	      -> server (this package)     the peer
//	        -> the runner below
//
// Everything the two halves share — message parts, task states, artifacts — comes
// from core/internal/a2awire, so a mapping that is right on one side and wrong on
// the other shows up here and nowhere else.

import (
	"context"
	"fmt"
	"iter"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	agenta2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/agent-host"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/builtin"
	"github.com/GizClaw/flowcraft/core/deploy/protocols/a2a"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/runtime/backends"
)

// scripted yields a fixed event sequence and finishes. It stands in for the
// model loop at the far end of the chain.
type scripted struct{ events []agent.Event }

func (r scripted) State() agent.TaskState { return agent.StateSubmitted }

func (r scripted) Run(context.Context, message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		for _, event := range r.events {
			if !yield(event, nil) {
				return
			}
		}
	}
}

func (r scripted) Cancel(context.Context, string) error { return nil }
func (r scripted) Close() error                         { return nil }

// servedPeer exposes one runner over A2A and returns the listening server.
func servedPeer(t testing.TB, runner agent.Runner) *httptest.Server {
	t.Helper()
	table, err := agent.NewAgent(agent.AgentOptions{
		NewRunner: func(context.Context, string, string) (agent.Runner, error) {
			return runner, nil
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = table.Close() })
	srv, err := a2a.New(a2a.Config{
		Name:     "peer",
		Settings: settingsJSON(map[string]any{"card": map[string]any{"version": "1.0.0", "url": "https://peer.test"}}),
		Agent:    table,
	})
	if err != nil {
		t.Fatalf("a2a.New: %v", err)
	}
	listener := httptest.NewServer(srv)
	t.Cleanup(listener.Close)
	return listener
}

// roundTrip wires the whole chain: a peer serving runner, a deployment that
// proxies that peer, that deployment exposed over A2A, and a client driving it.
func roundTrip(t testing.TB, runner agent.Runner) *agenta2a.Runner {
	t.Helper()
	return dial(t, roundTripServer(t, runner), "round-trip")
}

// roundTripServer builds the chain up to the listening server, so a benchmark can
// pay for a fresh client (a card fetch and a new session) on every turn.
func roundTripServer(t testing.TB, runner agent.Runner) *httptest.Server {
	t.Helper()
	peerServer := servedPeer(t, runner)

	manager := newManager(t, builtin.Kinds(), host.Bindings{
		RuntimeOptions:  []runtime.Option{stubDriver()},
		SandboxBackends: backends.All(),
	})
	spec := deploy.Spec{
		Name:     "backend",
		Engine:   deploy.EngineSpec{Kind: "a2a", Settings: settingsJSON(map[string]any{"peer": peerServer.URL})},
		Protocol: &deploy.ProtocolSpec{Kind: "a2a", Settings: settingsJSON(map[string]any{"card": map[string]any{"version": "1.0.0", "url": "http://backend.invalid"}})},
	}
	if err := manager.Add(t.Context(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	handler, ok := manager.Handler("backend")
	if !ok {
		t.Fatal("the agent exposes no handler")
	}
	served := httptest.NewServer(handler)
	t.Cleanup(served.Close)
	return served
}

// dial attaches an A2A client to a served deployment.
func dial(t testing.TB, served *httptest.Server, session string) *agenta2a.Runner {
	t.Helper()
	client, err := agenta2a.New(t.Context(), agenta2a.Config{
		Peer:      served.URL,
		SessionID: session,
	})
	if err != nil {
		t.Fatalf("agenta2a.New: %v", err)
	}
	return client
}

// answer is what a caller sees at the far end of the chain.
type answer struct {
	text      string
	state     agent.TaskState
	artifacts []agent.Artifact
	kinds     map[agent.EventKind]int
}

func (a *answer) collect(t testing.TB, client *agenta2a.Runner) {
	t.Helper()
	a.kinds = make(map[agent.EventKind]int)
	for event, err := range client.Run(t.Context(), message.NewTextMessage(message.RoleUser, "go")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if event == nil {
			continue
		}
		a.kinds[event.Kind()]++
		switch typed := event.(type) {
		case agent.Text:
			a.text += typed.Text
		case agent.Status:
			a.state = typed.State
		case agent.ArtifactWritten:
			a.artifacts = append(a.artifacts, typed.Artifact)
		}
	}
}

// A produced artifact crosses both hops with its identity and its locator
// intact: the server writes it as a data part, the client reads it back.
func TestRoundTripCarriesAnArtifact(t *testing.T) {
	produced := agent.Artifact{
		ID:        "art-1",
		Name:      "report.md",
		MediaType: "text/markdown",
		Blob:      "blob-ref-1",
	}
	client := roundTrip(t, scripted{events: []agent.Event{
		agent.Status{State: agent.StateWorking},
		agent.Text{Text: "done"},
		agent.ArtifactWritten{Artifact: produced},
		agent.Status{State: agent.StateCompleted},
	}})

	var got answer
	got.collect(t, client)

	if len(got.artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want the one the runner produced", got.artifacts)
	}
	if got.artifacts[0] != produced {
		t.Errorf("artifact = %+v, want %+v", got.artifacts[0], produced)
	}
	if got.state != agent.StateCompleted {
		t.Errorf("final state = %s, want completed", got.state)
	}
}

// A paused task keeps its question: the reason travels as the status message, so
// a caller can answer instead of waiting for an answer that will not come.
func TestRoundTripCarriesTheQuestion(t *testing.T) {
	client := roundTrip(t, scripted{events: []agent.Event{
		agent.Status{State: agent.StateWorking},
		agent.Status{State: agent.StateInputRequired, Reason: "which seat?"},
	}})

	var got answer
	got.collect(t, client)

	if got.state != agent.StateInputRequired {
		t.Errorf("final state = %s, want input-required", got.state)
	}
	if got.text != "which seat?" {
		t.Errorf("text = %q, want the question (events: %v)", got.text, got.kinds)
	}
}

// The protocol has no vocabulary for the agent's working: tool calls, results,
// usage and reasoning stay with the peer. What must not happen is the answer
// disappearing with them.
func TestRoundTripDropsWhatTheWireCannotCarry(t *testing.T) {
	client := roundTrip(t, scripted{events: []agent.Event{
		agent.Status{State: agent.StateWorking},
		agent.ToolCallStart{Call: message.ToolCall{ID: "call-1", Name: "read_file"}},
		agent.ToolResult{Result: message.ToolResult{CallID: "call-1"}},
		agent.UsageEvent{Usage: usage.Usage{InputTokens: 7, OutputTokens: 3}},
		agent.Reasoning{Text: "thinking out loud"},
		agent.Text{Text: "the answer"},
		agent.Status{State: agent.StateCompleted},
	}})

	var got answer
	got.collect(t, client)

	if got.text != "the answer" {
		t.Errorf("text = %q, want the answer that followed the dropped events", got.text)
	}
	for _, kind := range []agent.EventKind{agent.KindToolCall, agent.KindToolResult, agent.KindUsage, agent.KindReasoning} {
		if got.kinds[kind] != 0 {
			t.Errorf("%s crossed the wire %d times, want none", kind, got.kinds[kind])
		}
	}
}

// One hop: a client against the peer directly, with no deployment proxying in
// between. The difference against BenchmarkRoundTripTurn is what the second
// adapter costs — the same turn translated twice.
func BenchmarkSingleHopTurn(b *testing.B) {
	served := servedPeer(b, scripted{events: []agent.Event{
		agent.Status{State: agent.StateWorking},
		agent.Text{Text: "answer"},
		agent.Status{State: agent.StateCompleted},
	}})

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		client := dial(b, served, fmt.Sprintf("one-hop-%d", i))
		var got answer
		got.collect(b, client)
		if got.text != "answer" {
			b.Fatalf("text = %q, want the answer", got.text)
		}
	}
}

// The first turn on a session costs a card fetch; later turns do not. The unit
// measured here is the cold one: a fresh session, its card fetch, one message and
// the stream back, through two HTTP hops and two protocol adapters.
func BenchmarkRoundTripTurn(b *testing.B) {
	served := roundTripServer(b, scripted{events: []agent.Event{
		agent.Status{State: agent.StateWorking},
		agent.Text{Text: "answer"},
		agent.Status{State: agent.StateCompleted},
	}})

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		client := dial(b, served, fmt.Sprintf("bench-%d", i))
		var got answer
		got.collect(b, client)
		if got.text != "answer" {
			b.Fatalf("text = %q, want the answer", got.text)
		}
	}
}

// The same turn with an artifact produced: this is where the wire mapping does
// real work, because the artifact crosses both adapters as a data part.
func BenchmarkRoundTripArtifactTurn(b *testing.B) {
	served := roundTripServer(b, scripted{events: []agent.Event{
		agent.Status{State: agent.StateWorking},
		agent.Text{Text: "answer"},
		agent.ArtifactWritten{Artifact: agent.Artifact{
			ID: "art-1", Name: "report.md", MediaType: "text/markdown", Blob: "blob-ref",
		}},
		agent.Status{State: agent.StateCompleted},
	}})

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		client := dial(b, served, fmt.Sprintf("bench-art-%d", i))
		var got answer
		got.collect(b, client)
		if len(got.artifacts) != 1 {
			b.Fatalf("artifacts = %+v, want one", got.artifacts)
		}
	}
}
