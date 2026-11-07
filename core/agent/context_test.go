package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// recordPolicy keeps a window of the history and records what it was handed.
type recordPolicy struct {
	keep     int
	state    json.RawMessage
	contexts [][]message.Message
	states   []string
}

func (p *recordPolicy) Project(_ context.Context, in Projection) (Projected, error) {
	p.states = append(p.states, string(in.State))
	window := in.History
	if p.keep > 0 && len(window) > p.keep {
		window = window[len(window)-p.keep:]
	}
	p.contexts = append(p.contexts, window)
	return Projected{Context: window, State: p.state}, nil
}

// The default must not change what the model sees: the whole Task minus the input.
func TestDefaultPolicySendsTheWholeHistory(t *testing.T) {
	provider := &scriptedProvider{steps: []inference.GenerateResponse{textResponse("ok")}}
	session, _ := newTestSession(t, provider)
	seedHistory(t, session, []message.Message{
		message.NewTextMessage(message.RoleUser, "first"),
		message.NewTextMessage(message.RoleAssistant, "answer"),
	})

	for _, err := range session.Run(context.Background(), Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "second"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if len(provider.seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(provider.seen))
	}
	if got := len(provider.seen[0].Context); got != 2 {
		t.Errorf("context = %d messages, want the 2 seeded ones", got)
	}
	if got := provider.seen[0].Input.Content.Text(); got != "second" {
		t.Errorf("input = %q, want the new message", got)
	}
}

// A policy decides what the model sees; the Task's own history is untouched, so
// a restore still replays everything.
func TestContextPolicyNarrowsWhatTheModelSees(t *testing.T) {
	policy := &recordPolicy{keep: 1}
	provider := &scriptedProvider{steps: []inference.GenerateResponse{textResponse("ok")}}
	session, _ := newTestSession(t, provider, WithContextPolicy(policy))
	seedHistory(t, session, []message.Message{
		message.NewTextMessage(message.RoleUser, "first"),
		message.NewTextMessage(message.RoleAssistant, "answer"),
	})

	for _, err := range session.Run(context.Background(), Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "second"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if got := len(provider.seen[0].Context); got != 1 {
		t.Errorf("context = %d messages, want the 1 the policy kept", got)
	}
	// Two seeded messages, the input, and the assistant's answer: the policy
	// narrowed the request, not the log.
	if got := len(session.Task().History); got != 4 {
		t.Errorf("task history = %d messages, want the full 4", got)
	}
}

// A policy's durable state rides the checkpoint's own section and comes back on
// restore, so a restarted process makes the same projection.
func TestContextPolicyStateSurvivesARestore(t *testing.T) {
	state := json.RawMessage(`{"summary":"older turns"}`)
	policy := &recordPolicy{state: state}
	provider := &scriptedProvider{steps: []inference.GenerateResponse{textResponse("ok")}}
	session, rt := newTestSession(t, provider, WithContextPolicy(policy))
	for _, err := range session.Run(context.Background(), Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "hi"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	snapshot := session.Snapshot()
	if got := string(snapshot.Sections[sectionContextPolicy]); got != string(state) {
		t.Fatalf("checkpoint section = %s, want %s", got, state)
	}

	restored, err := Restore(rt, snapshot, WithContextPolicy(&recordPolicy{}))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := string(restored.policyState); got != string(state) {
		t.Errorf("restored state = %s, want %s", got, state)
	}
}
