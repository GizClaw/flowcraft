package a2a

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"

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

func TestToA2AMapsAnswerTextAndLifecycle(t *testing.T) {
	info := testInfo()
	event, ok := toA2A(info, agent.Text{Text: "hi"})
	if !ok {
		t.Fatal("answer text was dropped")
	}
	update, isUpdate := event.(*a2a.TaskStatusUpdateEvent)
	if !isUpdate {
		t.Fatalf("event = %T, want a status update", event)
	}
	if update.Status.State != a2a.TaskStateWorking || update.Status.Message == nil {
		t.Errorf("update = %+v, want working with a message", update.Status)
	}

	event, ok = toA2A(info, agent.Status{State: agent.StateCompleted})
	if !ok {
		t.Fatal("lifecycle was dropped")
	}
	if status := event.(*a2a.TaskStatusUpdateEvent).Status; status.State != a2a.TaskStateCompleted {
		t.Errorf("state = %q, want completed", status.State)
	}

	// Internal working has no wire form.
	if _, ok := toA2A(info, agent.ToolCallStart{}); ok {
		t.Error("a tool call crossed the wire")
	}
}

// A content type the core has no part for is an error, not a dropped part.
func TestFromA2APartRejectsWhatTheCoreCannotCarry(t *testing.T) {
	if _, err := fromA2APart(nil); err == nil {
		t.Error("a nil part was accepted")
	}
	raw := &a2a.Part{Content: a2a.Raw([]byte("bytes")), MediaType: "image/png"}
	if _, err := fromA2APart(raw); err == nil {
		t.Error("inline bytes were accepted")
	}
}

func TestFromA2AMessageCarriesText(t *testing.T) {
	msg, err := fromA2AMessage(a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")))
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
		Sessions: func(context.Context, *a2asrv.ExecutorContext) (*agent.Session, error) {
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
		Sessions: func(context.Context, *a2asrv.ExecutorContext) (*agent.Session, error) { return nil, nil },
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
