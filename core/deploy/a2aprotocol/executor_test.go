package a2aprotocol

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Answer text crosses as artifact chunks: a status update carries the latest
// status, so one message per delta would shred the answer across the history.
//
// One chunk is held back so the round's last chunk can carry LastChunk: the
// protocol rejects an artifact update without parts.
func TestTextBecomesArtifactChunksPerRound(t *testing.T) {
	stream := newTextStream(testInfo(), nil)
	var events []a2a.Event
	for _, event := range []agent.Event{
		agent.StreamStart{Round: 1},
		agent.Text{Text: "hel"},
		agent.Text{Text: "lo"},
		agent.Status{State: agent.StateCompleted},
	} {
		events = append(events, stream.events(event)...)
	}
	events = append(events, stream.close()...)
	if len(events) != 3 {
		t.Fatalf("events = %d, want two chunks and the status", len(events))
	}
	first, ok := events[0].(*a2a.TaskArtifactUpdateEvent)
	if !ok {
		t.Fatalf("event = %T, want an artifact update", events[0])
	}
	if first.Append || first.LastChunk {
		t.Errorf("first chunk append=%v last=%v, want a fresh open chunk", first.Append, first.LastChunk)
	}
	if text := first.Artifact.Parts[0].Text(); text != "hel" {
		t.Errorf("first chunk = %q, want hel", text)
	}
	second := events[1].(*a2a.TaskArtifactUpdateEvent)
	if !second.Append || !second.LastChunk {
		t.Errorf("second chunk append=%v last=%v, want the closing append",
			second.Append, second.LastChunk)
	}
	if second.Artifact.ID != first.Artifact.ID {
		t.Errorf("second chunk changed the artifact id")
	}
	if text := second.Artifact.Parts[0].Text(); text != "lo" {
		t.Errorf("second chunk = %q, want lo", text)
	}
	status, ok := events[2].(*a2a.TaskStatusUpdateEvent)
	if !ok || status.Status.State != a2a.TaskStateCompleted {
		t.Errorf("last event = %T, want completed", events[2])
	}
}

// Each model round is its own artifact: a tool round that follows must not
// append to the answer that already finished.
func TestANewRoundClosesThePreviousArtifact(t *testing.T) {
	stream := newTextStream(testInfo(), nil)
	if events := stream.events(agent.Text{Text: "one"}); len(events) != 0 {
		t.Fatalf("a held chunk crossed early: %+v", events)
	}
	opened := stream.events(agent.Text{Text: "two"})
	if len(opened) != 1 {
		t.Fatalf("second chunk events = %d", len(opened))
	}
	firstID := opened[0].(*a2a.TaskArtifactUpdateEvent).Artifact.ID
	closing := stream.events(agent.StreamStart{Round: 2})
	if len(closing) != 1 || !closing[0].(*a2a.TaskArtifactUpdateEvent).LastChunk {
		t.Fatalf("a new round did not close the previous artifact: %+v", closing)
	}
	if text := closing[0].(*a2a.TaskArtifactUpdateEvent).Artifact.Parts[0].Text(); text != "two" {
		t.Errorf("closing chunk = %q, want the held text", text)
	}
	next := stream.events(agent.Text{Text: "three"})
	if len(next) != 0 {
		t.Fatalf("the new round crossed before closing: %+v", next)
	}
	final := stream.close()
	if len(final) != 1 {
		t.Fatalf("final close events = %d", len(final))
	}
	secondID := final[0].(*a2a.TaskArtifactUpdateEvent).Artifact.ID
	if firstID == secondID {
		t.Errorf("both rounds share the artifact %q", firstID)
	}
}

// The host decides how a blob becomes fetchable; without a resolver the
// reference itself crosses, which a client can at least act on.
func TestArtifactsUseTheHostResolver(t *testing.T) {
	stream := newTextStream(testInfo(), func(ref string) string { return "https://files.test/" + ref })
	events := stream.events(agent.ArtifactWritten{Artifact: agent.Artifact{
		ID: "a1", Name: "report", MediaType: "text/plain", Blob: "sha256:1",
	}})
	update, ok := events[0].(*a2a.TaskArtifactUpdateEvent)
	if !ok || !update.LastChunk {
		t.Fatalf("event = %T, want a whole artifact", events[0])
	}
	if update.Artifact.ID != "a1" || update.Artifact.Name != "report" {
		t.Errorf("artifact = %+v, want its id and name kept", update.Artifact)
	}
	url, ok := update.Artifact.Parts[0].Content.(a2a.URL)
	if !ok || string(url) != "https://files.test/sha256:1" {
		t.Errorf("part = %#v, want the resolved URL", update.Artifact.Parts[0].Content)
	}
}

func TestArtifactsFallBackToAReference(t *testing.T) {
	stream := newTextStream(testInfo(), nil)
	events := stream.events(agent.ArtifactWritten{Artifact: agent.Artifact{
		Blob: "sha256:1", MediaType: "text/plain",
	}})
	update := events[0].(*a2a.TaskArtifactUpdateEvent)
	data, ok := update.Artifact.Parts[0].Content.(a2a.Data)
	if !ok {
		t.Fatalf("part = %T, want a data part", update.Artifact.Parts[0].Content)
	}
	fields, ok := data.Value.(map[string]string)
	if !ok || fields["blob"] != "sha256:1" {
		t.Errorf("data = %#v, want the blob reference", data.Value)
	}
	if update.Artifact.ID == "" {
		t.Error("the artifact has no id")
	}
}

func TestInternalEventsDoNotCrossTheWire(t *testing.T) {
	stream := newTextStream(testInfo(), nil)
	if events := stream.events(agent.ToolCallStart{}); len(events) != 0 {
		t.Errorf("a tool call crossed the wire: %+v", events)
	}
}

// An execution without a message has nothing to run; the protocol gets an error
// rather than an empty task.
func TestExecuteRefusesAnEmptyExecution(t *testing.T) {
	executor, err := newExecutor(executorConfig{
		Card:  cardConfig{Name: "x", Version: "1", URL: "https://x"},
		Agent: tableFor(t, &completedRunner{}),
	})
	if err != nil {
		t.Fatalf("newExecutor: %v", err)
	}
	for _, exec := range []*a2asrv.ExecutorContext{nil, {}} {
		var seen error
		for _, err := range executor.Execute(context.Background(), exec) {
			seen = err
		}
		if seen == nil {
			t.Errorf("execution %+v was accepted", exec)
		}
	}
}

// A run whose step failed still reports the failure: the loop could not report
// it as a status, so the adapter says so instead of closing silently.
func TestExecuteReportsAFailedStep(t *testing.T) {
	executor, err := newExecutor(executorConfig{
		Card:  cardConfig{Name: "x", Version: "1", URL: "https://x"},
		Agent: tableFor(t, &failedRunner{err: errors.New("model down")}),
	})
	if err != nil {
		t.Fatalf("newExecutor: %v", err)
	}
	var failed bool
	for event, err := range executor.Execute(context.Background(), execContext()) {
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		update, ok := event.(*a2a.TaskStatusUpdateEvent)
		if ok && update.Status.State == a2a.TaskStateFailed {
			failed = true
		}
	}
	if !failed {
		t.Error("a failed step was not reported to the client")
	}
}
