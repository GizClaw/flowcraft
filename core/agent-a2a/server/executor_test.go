package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a/internal/wire"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

func testInfo() a2a.TaskInfo {
	return a2a.TaskInfo{TaskID: "task-1", ContextID: "conv-1"}
}

// Streaming is a fact about the loop; modes and skills are promises and stay
// empty until a deployment makes them.
func TestCardClaimsOnlyWhatItCan(t *testing.T) {
	card, err := NewCard(CardConfig{Name: "storyteller", Version: "1.0.0", URL: "https://example.test"})
	if err != nil {
		t.Fatalf("NewCard: %v", err)
	}
	if !card.Capabilities.Streaming {
		t.Error("streaming = false, want true")
	}
	if len(card.DefaultInputModes) != 0 || len(card.Skills) != 0 {
		t.Errorf("modes/skills = %v/%v, want none: nothing was declared",
			card.DefaultInputModes, card.Skills)
	}
	if len(card.SupportedInterfaces) != 1 {
		t.Errorf("interfaces = %d, want the declared endpoint", len(card.SupportedInterfaces))
	}
}

func TestCardRequiresTheDeclaredFacts(t *testing.T) {
	if _, err := NewCard(CardConfig{Version: "1", URL: "https://x"}); err == nil {
		t.Error("a card without a name was accepted")
	}
	if _, err := NewCard(CardConfig{Name: "x", URL: "https://x"}); err == nil {
		t.Error("a card without a version was accepted")
	}
}

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

// Inline bytes land in the part that matches them; anything else becomes an
// inline file. A nil part is still refused.
func TestFromA2APartCarriesInlineBytes(t *testing.T) {
	if _, err := wire.FromPart(nil); err == nil {
		t.Error("a nil part was accepted")
	}
	image, err := wire.FromPart(&a2a.Part{Content: a2a.Raw([]byte("png")), MediaType: "image/png"})
	if err != nil {
		t.Fatalf("image raw part: %v", err)
	}
	part, ok := image.(message.ImagePart)
	if !ok || string(part.Source.Bytes()) != "png" {
		t.Fatalf("image part = %#v, want inline bytes", image)
	}
	file, err := wire.FromPart(&a2a.Part{Content: a2a.Raw([]byte("pdf")), MediaType: "application/pdf", Filename: "a.pdf"})
	if err != nil {
		t.Fatalf("file raw part: %v", err)
	}
	filePart, ok := file.(message.FilePart)
	if !ok || string(filePart.Source.Bytes()) != "pdf" || filePart.Name != "a.pdf" {
		t.Fatalf("file part = %#v, want an inline file", file)
	}
}

func TestFromA2AMessageCarriesMetadata(t *testing.T) {
	msg := &a2a.Message{
		ID:   "m1",
		Role: a2a.MessageRoleUser,
		Parts: []*a2a.Part{
			{Content: a2a.Text("hi"), Metadata: map[string]any{"alt": "x"}},
		},
		ReferenceTasks: []a2a.TaskID{"task-1"},
		Extensions:     []string{"https://example.test/ext"},
		Metadata:       map[string]any{"note": map[string]any{"k": float64(1)}},
	}
	core, err := wire.FromMessage(msg)
	if err != nil {
		t.Fatalf("fromA2AMessage: %v", err)
	}
	if core.Metadata == nil {
		t.Fatal("the metadata was dropped")
	}
	if len(core.Metadata.ReferenceTasks) != 1 || core.Metadata.ReferenceTasks[0] != "task-1" {
		t.Errorf("reference tasks = %v", core.Metadata.ReferenceTasks)
	}
	if len(core.Metadata.Extensions) != 1 || core.Metadata.Extensions[0] != "https://example.test/ext" {
		t.Errorf("extensions = %v", core.Metadata.Extensions)
	}
	if got := string(core.Metadata.Attributes["note"]); !strings.Contains(got, `"k":1`) {
		t.Errorf("note = %s, want the protocol metadata", got)
	}
	if got := string(core.Metadata.Attributes[wire.PartAttributeKey(0)]); !strings.Contains(got, `"alt":"x"`) {
		t.Errorf("part metadata = %s, want it kept by position", got)
	}
}

func TestFromA2AMessageCarriesText(t *testing.T) {
	msg, err := wire.FromMessage(a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")))
	if err != nil {
		t.Fatalf("fromA2AMessage: %v", err)
	}
	if msg.Content.Text() != "hello" {
		t.Errorf("content = %q, want hello", msg.Content.Text())
	}
}

func TestExecuteReportsBuilderFailure(t *testing.T) {
	executor, err := New(Config{
		Card: CardConfig{Name: "x", Version: "1", URL: "https://x"},
		Sessions: func(context.Context, *a2asrv.ExecutorContext) (agent.Runner, error) {
			return nil, errors.New("no runtime")
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	exec := &a2asrv.ExecutorContext{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
		TaskID:  "task-1",
	}
	var seen error
	for _, err := range executor.Execute(context.Background(), exec) {
		seen = err
	}
	if seen == nil || !strings.Contains(seen.Error(), "no runtime") {
		t.Errorf("err = %v, want the builder failure", seen)
	}
}

func TestCancelReportsTheRequestedState(t *testing.T) {
	executor, err := New(Config{
		Card:     CardConfig{Name: "x", Version: "1", URL: "https://x"},
		Sessions: func(context.Context, *a2asrv.ExecutorContext) (agent.Runner, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	exec := &a2asrv.ExecutorContext{TaskID: "task-1"}
	var state a2a.TaskState
	for event, err := range executor.Cancel(context.Background(), exec) {
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		state = event.(*a2a.TaskStatusUpdateEvent).Status.State
	}
	if state != a2a.TaskStateCanceled {
		t.Errorf("state = %q, want canceled", state)
	}
}
