package a2a_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/agent/a2a"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// connect opens one session over a peer, which is how a host does it: the program is the
// deployment's, the driver is the conversation's.
func connect(t *testing.T, peerURL, sessionID string) agent.Driver {
	t.Helper()
	program, err := a2a.New(context.Background(), a2a.Config{Peer: peerURL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = program.Close() })
	driver, err := program.Open(agent.SessionInfo{SessionID: sessionID})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	return driver
}

// request is one turn of a session that persists nothing.
func request(sessionID, text string) agent.Request {
	return agent.Request{
		Task:    agent.Task{ID: "task-" + sessionID, SessionID: sessionID, State: agent.StateWorking},
		Input:   message.NewTextMessage(message.RoleUser, text),
		Binding: agenttest.BindingFor(sessionID),
	}
}

// turn drains one turn and returns what a caller folds: the answer text, the artifacts, the
// terminal status and the first error.
type turnResult struct {
	answer    strings.Builder
	artifacts []agent.Artifact
	status    agent.Status
	err       error
}

// resumeTurn is runTurn through the resume verb.
func resumeTurn(t *testing.T, driver agent.Resumer, req agent.Request) turnResult {
	t.Helper()
	var got turnResult
	for event, err := range driver.Resume(context.Background(), req) {
		if err != nil {
			if got.err == nil {
				got.err = err
			}
			continue
		}
		switch typed := event.(type) {
		case agent.Text:
			got.answer.WriteString(typed.Text)
		case agent.Status:
			got.status = typed
		}
	}
	return got
}

func runTurn(t *testing.T, driver agent.Driver, req agent.Request) turnResult {
	t.Helper()
	var got turnResult
	for event, err := range driver.Run(context.Background(), req) {
		if err != nil {
			if got.err == nil {
				got.err = err
			}
			continue
		}
		switch typed := event.(type) {
		case agent.Text:
			got.answer.WriteString(typed.Text)
		case agent.ArtifactWritten:
			got.artifacts = append(got.artifacts, typed.Artifact)
		case agent.Status:
			if typed.State.Terminal() || typed.Reason != "" {
				got.status = typed
			}
		}
	}
	return got
}

// A remote agent satisfies the same contract a local engine does.
func TestThePeerSatisfiesTheDriverContract(t *testing.T) {
	agenttest.DriverSuite(t, func(t *testing.T) agent.Program {
		server, _ := peer(t,
			agent.Status{State: agent.StateWorking},
			agent.Text{Text: "hi"},
			agent.Status{State: agent.StateCompleted},
		)
		program, err := a2a.New(context.Background(), a2a.Config{Peer: server.URL})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return program
	})
}

// The peer's answer arrives as text even though the wire carries it as artifact chunks: the
// driver hands its caller core events, not protocol ones.
func TestRunCarriesTheRemoteAnswer(t *testing.T) {
	server, _ := peer(t,
		agent.Status{State: agent.StateWorking},
		agent.Text{Text: "hello "},
		agent.Text{Text: "world"},
		agent.Status{State: agent.StateCompleted},
	)
	got := runTurn(t, connect(t, server.URL, "conv-1"), request("conv-1", "hi"))
	if got.err != nil {
		t.Fatalf("Run: %v", got.err)
	}
	if answer := got.answer.String(); answer != "hello world" {
		t.Fatalf("answer = %q, want the peer's text", answer)
	}
	if got.status.State != agent.StateCompleted {
		t.Fatalf("status = %s, want completed", got.status.State)
	}
}

// A produced artifact crosses with its identity and locator intact, and a paused task reports the
// question the peer put on its status.
func TestArtifactsAndTheQuestionCrossTheProtocol(t *testing.T) {
	server, _ := peer(t,
		agent.Status{State: agent.StateWorking},
		agent.ArtifactWritten{Artifact: agent.Artifact{
			ID: "a1", Name: "report", MediaType: "text/plain", Blob: "sha256:1",
		}},
		agent.Status{State: agent.StateInputRequired, Reason: "which one?"},
	)
	got := runTurn(t, connect(t, server.URL, "conv-1"), request("conv-1", "hi"))
	if got.err != nil {
		t.Fatalf("Run: %v", got.err)
	}
	if len(got.artifacts) != 1 {
		t.Fatalf("artifacts = %d, want one", len(got.artifacts))
	}
	if a := got.artifacts[0]; a.ID != "a1" || a.Name != "report" || a.Blob != "sha256:1" {
		t.Errorf("artifact = %+v, want the peer's id, name and reference", a)
	}
	if got.status.State != agent.StateInputRequired {
		t.Fatalf("status = %s, want input-required", got.status.State)
	}
	if !strings.Contains(got.status.Reason, "which one?") {
		t.Errorf("reason = %q, want the peer's question", got.status.Reason)
	}
	if answer := got.answer.String(); answer != "" {
		t.Errorf("answer = %q, want the question kept out of it", answer)
	}
}

// The peer's context outlives the task: a restarted process continues the session even though the
// task it belonged to already finished. The driver owns a section of the host's task, and the
// section is where that identity survives.
func TestRestartKeepsTheRemoteContext(t *testing.T) {
	server, engine := peer(t,
		agent.Status{State: agent.StateWorking},
		agent.Status{State: agent.StateCompleted},
	)
	store := agenttest.NewMemStore()
	first := connect(t, server.URL, "conv-1")
	if got := runTurn(t, first, withStore(request("conv-1", "hi"), store)); got.err != nil {
		t.Fatalf("Run: %v", got.err)
	}
	before := engine.lastContext()
	if before == "" {
		t.Fatal("the peer assigned no context")
	}

	// A new program and driver over the same store, as a restarted process would have.
	second := connect(t, server.URL, "conv-1")
	if got := runTurn(t, second, withStore(request("conv-1", "again"), store)); got.err != nil {
		t.Fatalf("Run after restart: %v", got.err)
	}
	if after := engine.lastContext(); after != before {
		t.Errorf("context after restart = %q, want the peer's %q", after, before)
	}
}

// A task waiting for the caller survives a restart: the peer's task id is in the section, so the
// next turn answers that task instead of starting a new one — and the driver knows it is waiting
// before it has spoken to the peer, because it reads the state from the store on the way in.
func TestThePausedTaskSurvivesARestart(t *testing.T) {
	server, engine := peer(t,
		agent.Status{State: agent.StateWorking},
		agent.Status{State: agent.StateInputRequired},
	)
	store := agenttest.NewMemStore()
	first := connect(t, server.URL, "conv-1")
	if got := runTurn(t, first, withStore(request("conv-1", "hi"), store)); got.err != nil {
		t.Fatalf("Run: %v", got.err)
	}
	checkpoint, err := store.Load(context.Background(), "task-conv-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if checkpoint.Task.State != agent.StateInputRequired {
		t.Fatalf("task state = %s, want input-required", checkpoint.Task.State)
	}
	section, ok := checkpoint.Sections["a2a"]
	if !ok {
		t.Fatal("the peer's identity was not written to the task's section")
	}
	if !strings.Contains(string(section), "task_id") {
		t.Fatalf("section = %s, want the peer's task id", section)
	}

	second := connect(t, server.URL, "conv-1")
	// A new message would be taken as the answer to the peer's question, so Run refuses it once
	// the restored state is known.
	if got := runTurn(t, second, withStore(request("conv-1", "that one"), store)); !errors.Is(got.err, agent.ErrAwaitingResume) {
		t.Fatalf("Run after restart = %v, want ErrAwaitingResume", got.err)
	}
	resumer, ok := second.(agent.Resumer)
	if !ok {
		t.Fatal("the a2a driver cannot resume")
	}
	if got := resumeTurn(t, resumer, withStore(request("conv-1", "that one"), store)); got.err != nil {
		t.Fatalf("Resume after restart: %v", got.err)
	}
	taskID := engine.lastTaskID()
	if taskID == "" {
		t.Fatal("the peer saw no task id on the resumed message")
	}
}

// Cancel is the protocol's own: dropping the context stops the local read and leaves the peer
// running, which is why the driver implements agent.Canceller at all.
//
// The cancel has to target a task that is *still running*. Scripting a terminal status and then
// cancelling asked a different question: the peer's store would already say the task is over, the
// protocol refuses to cancel such a task, and whether the refusal won was a race between the
// server's store write and the next request — which is what made this test fail on CI while
// passing locally. So the peer's engine blocks after its first status, the cancel lands while the
// run is in flight, and the assertion is about the peer having been told.
func TestCancelReachesThePeer(t *testing.T) {
	server, engine := peer(t, agent.Status{State: agent.StateWorking})
	engine.started = make(chan struct{})
	engine.block = make(chan struct{})

	driver := connect(t, server.URL, "conv-1")
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Drained without assertions: a t.Fatalf here would run on a goroutine that is not the
		// test's, which the testing package does not allow.
		for range driver.Run(ctx, request("conv-1", "hi")) {
		}
	}()

	select {
	case <-engine.started:
	case <-ctx.Done():
		t.Fatal("the peer never started the run")
	}

	canceller, ok := driver.(agent.Canceller)
	if !ok {
		t.Fatal("the peer driver must be able to forward a cancellation")
	}
	// A cancel that arrives before the peer has named its task is a no-op by contract — there is
	// nothing to cancel yet — so the operator's retry is what makes this deterministic: the
	// alternative is a test that races the first stream event.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := canceller.Cancel(ctx, "user stopped"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if engine.wasCancelled() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the peer never saw the cancellation")
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the cancelled run never finished")
	}
}

// The protocol has no steering message, so the driver must not pretend to accept input it could
// never deliver.
func TestTheDriverDoesNotSteer(t *testing.T) {
	server, _ := peer(t, agent.Status{State: agent.StateCompleted})
	driver := connect(t, server.URL, "conv-1")
	if _, ok := driver.(agent.Steerer); ok {
		t.Fatal("a peer driver advertised steering; the protocol has no such message")
	}
}

// A config that names no peer is refused where it is read, not on the first turn.
func TestAConfigWithoutAPeerIsRefused(t *testing.T) {
	if _, err := a2a.New(context.Background(), a2a.Config{}); err == nil {
		t.Fatal("a config without a peer was accepted")
	}
}

// withStore attaches the store a host would pass, which is where the peer's identity is kept.
func withStore(req agent.Request, store agent.Store) agent.Request {
	req.Store = store
	req.Fingerprint = "sha256:deployment"
	return req
}

// TestAPausedTaskThroughTheTurnDecorator is the pairing the host relies on: a paused a2a task is
// refused by RunTurn and continued by ResumeTurn, with the same identity, failure box and
// checkpoint the host's turn puts around it.
func TestAPausedTaskThroughTheTurnDecorator(t *testing.T) {
	server, _ := peer(t,
		agent.Status{State: agent.StateWorking},
		agent.Status{State: agent.StateInputRequired},
	)
	store := agenttest.NewMemStore()
	driver := connect(t, server.URL, "conv-1")
	req := withStore(request("conv-1", "hi"), store)

	turn := func(start func(context.Context, agent.Request, agent.Driver) iter.Seq2[agent.Event, error]) turnResult {
		var got turnResult
		for event, err := range start(context.Background(), req, driver) {
			if err != nil {
				if got.err == nil {
					got.err = err
				}
				continue
			}
			switch typed := event.(type) {
			case agent.Text:
				got.answer.WriteString(typed.Text)
			case agent.Status:
				got.status = typed
			}
		}
		return got
	}

	if got := turn(agent.RunTurn); got.err != nil {
		t.Fatalf("first turn: %v", got.err)
	}
	if got := turn(agent.RunTurn); !errors.Is(got.err, agent.ErrAwaitingResume) {
		t.Fatalf("RunTurn on a paused task = %v, want ErrAwaitingResume", got.err)
	}
	if got := turn(agent.ResumeTurn); got.err != nil {
		t.Fatalf("ResumeTurn: %v", got.err)
	}
}
