package a2a

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// Streaming is a fact about the loop, so it is true without being declared;
// media modes and skills are promises, so they stay empty until a deployment
// makes them.
func TestCardDoesNotFabricateClaims(t *testing.T) {
	card, err := NewCard(CardConfig{Name: "storyteller"})
	if err != nil {
		t.Fatalf("NewCard: %v", err)
	}
	if !card.Capabilities.Streaming {
		t.Error("streaming = false, want true")
	}
	if card.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocol = %q, want %q", card.ProtocolVersion, ProtocolVersion)
	}
	if len(card.DefaultInputModes) != 0 || len(card.DefaultOutputModes) != 0 {
		t.Errorf("modes = %v/%v, want none: nothing was declared",
			card.DefaultInputModes, card.DefaultOutputModes)
	}
	if len(card.Skills) != 0 {
		t.Errorf("skills = %v, want none", card.Skills)
	}
}

func TestCardRejectsDuplicateSkills(t *testing.T) {
	_, err := NewCard(CardConfig{Name: "x", Skills: []Skill{
		{ID: "a", Name: "A"}, {ID: "a", Name: "B"},
	}})
	if err == nil {
		t.Fatal("duplicate skill id accepted")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error %q does not explain the duplicate", err)
	}
}

func TestConvertTaskCarriesTheLifecycle(t *testing.T) {
	task := agent.Task{
		ID:        "task-1",
		ContextID: "conv-1",
		State:     agent.StateCompleted,
		History: []message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
			message.NewTextMessage(message.RoleAssistant, "hello"),
		},
	}
	converted, err := ConvertTask(task)
	if err != nil {
		t.Fatalf("ConvertTask: %v", err)
	}
	if converted.Kind != "task" || converted.Status.State != "completed" {
		t.Errorf("task = %+v, want kind task and completed", converted)
	}
	if converted.ContextID != "conv-1" {
		t.Errorf("contextId = %q", converted.ContextID)
	}
	if len(converted.History) != 2 || converted.History[1].Role != "agent" {
		t.Errorf("history = %+v, want user then agent", converted.History)
	}
}

// A part with no wire form is an error, not a silent omission: a client cannot
// tell a dropped image from one that was never sent.
func TestConvertPartRejectsUnrepresentableKinds(t *testing.T) {
	if _, err := ConvertPart(message.ReasoningPart{Text: "thinking"}); err == nil {
		t.Fatal("reasoning part accepted")
	}
	if _, err := ConvertMessage(message.NewTextMessage(message.RoleSystem, "sys")); err == nil {
		t.Fatal("system message accepted")
	}
}

func TestUpdatesMapAnswerTextAndLifecycle(t *testing.T) {
	updates, final, err := Updates("task-1", "conv-1", agent.Text{Text: "hi"})
	if err != nil {
		t.Fatalf("Updates(text): %v", err)
	}
	if final || len(updates) != 1 || !updates[0].Append {
		t.Fatalf("updates = %+v final = %v, want one appending update", updates, final)
	}
	if updates[0].Status.Message.Parts[0].Text != "hi" {
		t.Errorf("text = %q", updates[0].Status.Message.Parts[0].Text)
	}

	updates, final, err = Updates("task-1", "conv-1", agent.Status{State: agent.StateCompleted})
	if err != nil {
		t.Fatalf("Updates(status): %v", err)
	}
	if !final {
		t.Error("completed did not end the stream")
	}
	if updates[0].Status.State != "completed" {
		t.Errorf("state = %q", updates[0].Status.State)
	}

	// input-required ends the stream too: the client must act before the task
	// moves again.
	if _, final, _ = Updates("t", "c", agent.Status{State: agent.StateInputRequired}); !final {
		t.Error("input-required did not end the stream")
	}
	// Internal working is not a wire event.
	if updates, _, _ := Updates("t", "c", agent.UsageEvent{}); len(updates) != 0 {
		t.Errorf("usage produced %d updates, want none", len(updates))
	}
}
