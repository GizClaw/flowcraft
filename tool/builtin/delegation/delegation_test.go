package delegation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"github.com/goccy/go-json"
)

// fake is the capability a host would fill: it records the request and returns what the case set up.
type fake struct {
	request   platform.DelegationRequest
	addressed string
	answered  platform.DelegationAnswer
	outcome   platform.DelegationOutcome
	err       error
}

func (f *fake) Delegate(_ context.Context, req platform.DelegationRequest) (platform.DelegationOutcome, error) {
	f.request = req
	return f.outcome, f.err
}

func (f *fake) Status(_ context.Context, session string) (platform.DelegationOutcome, error) {
	f.addressed = session
	return f.outcome, f.err
}

func (f *fake) Answer(_ context.Context, session string, answer platform.DelegationAnswer,
) (platform.DelegationOutcome, error) {
	f.addressed, f.answered = session, answer
	return f.outcome, f.err
}

func (f *fake) Cancel(_ context.Context, session, _ string) error {
	f.addressed = session
	return f.err
}

// bindingFor puts the capability in the pocket, which is where the tool reads it from.
func bindingFor(delegate platform.Delegation) tool.Binding {
	set := tool.NewCapSet()
	if delegate != nil {
		platform.DelegationSlot.Provide(set, delegate)
	}
	return tool.Bind(testSession{}, policy.Grant{}).WithCaps(set)
}

type testSession struct{}

func (testSession) ID() string            { return "delegation-test" }
func (testSession) Scratch() tool.Scratch { return testScratch{} }

type testScratch struct{}

func (testScratch) Load(string) (any, bool) { return nil, false }
func (testScratch) Store(string, any)       {}
func (testScratch) Delete(string)           {}

func call(t *testing.T, binding tool.Binding, arguments map[string]any) (map[string]any, error) {
	t.Helper()
	payload, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	content, err := delegateTool{}.Execute(context.Background(), binding, string(payload))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(content.Text()), &out); err != nil {
		t.Fatalf("decode %q: %v", content.Text(), err)
	}
	return out, nil
}

// The tool forwards what the model wrote and nothing more: the policy, the child's identity and how
// deep it may go are the host's, and a tool that second-guessed them would be a second policy.
func TestDelegateForwardsTheRequestAndReportsTheAnswer(t *testing.T) {
	capability := &fake{outcome: platform.DelegationOutcome{
		Status:  "completed",
		Session: "main/seat_1-1",
		Content: message.NewTextContent("the answer"),
		Chain:   "chain-1",
		Depth:   1,
	}}
	out, err := call(t, bindingFor(capability), map[string]any{
		"target": "seat_1", "input": "who are you?", "mode": ModeSync,
	})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if capability.request.Target != "seat_1" || capability.request.Input != "who are you?" {
		t.Fatalf("request = %+v", capability.request)
	}
	if capability.request.Mode != ModeSync {
		t.Fatalf("mode = %q", capability.request.Mode)
	}
	if out["content"] != "the answer" || out["session"] != "main/seat_1-1" {
		t.Fatalf("answer = %v", out)
	}
	if out["depth"].(float64) != 1 || out["chain"] != "chain-1" {
		t.Fatalf("lineage = %v", out)
	}
}

// An async hand-off has no answer yet: the address is the result, and a caller that got a fabricated
// empty answer would have no way to ask again.
func TestAsyncHandsOffTheSession(t *testing.T) {
	capability := &fake{outcome: platform.DelegationOutcome{
		Status:  "submitted",
		Session: "main/seat_2-1",
	}}
	out, err := call(t, bindingFor(capability), map[string]any{
		"target": "seat_2", "input": "work on it", "mode": ModeAsync,
	})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if _, ok := out["content"]; ok {
		t.Fatalf("answer = %v, want no content for a hand-off", out)
	}
	if out["session"] != "main/seat_2-1" {
		t.Fatalf("session = %v", out["session"])
	}
}

// A paused child reports its question, which is what the caller needs to answer.
func TestAPausedChildReportsItsQuestion(t *testing.T) {
	question := message.NewTextContent("which seat?")
	capability := &fake{outcome: platform.DelegationOutcome{
		Status:   "input-required",
		Session:  "main/referee-1",
		Question: &question,
	}}
	out, err := call(t, bindingFor(capability), map[string]any{
		"target": "referee", "input": "start the game",
	})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if out["question"] != "which seat?" {
		t.Fatalf("question = %v", out["question"])
	}
}

// The mode defaults to sync: a caller that named none asked for the answer.
func TestTheModeDefaultsToSync(t *testing.T) {
	capability := &fake{}
	if _, err := call(t, bindingFor(capability), map[string]any{"target": "a", "input": "b"}); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if capability.request.Mode != ModeSync {
		t.Fatalf("mode = %q, want sync", capability.request.Mode)
	}
}

// No capability is a session with no delegation policy, and it says so instead of pretending.
func TestWithoutTheCapabilityTheToolRefuses(t *testing.T) {
	if _, err := call(t, bindingFor(nil), map[string]any{"target": "a", "input": "b"}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want not-available", err)
	}
}

// A refusal from the host is forwarded as an error, not folded into an answer: "it was not allowed"
// and "it ran and produced nothing" must not look the same.
func TestARefusalIsNotAnAnswer(t *testing.T) {
	refusal := errdefs.Forbiddenf("host: this session may not delegate")
	capability := &fake{err: refusal}
	if _, err := call(t, bindingFor(capability), map[string]any{"target": "a", "input": "b"}); !errors.Is(err, refusal) {
		t.Fatalf("err = %v, want the host's refusal", err)
	}
}

// The three follow-up tools address the child by the id the delegate tool returned, and pass the
// arguments through unchanged: the host decides whether this caller may touch that child.
func TestTheFollowUpToolsAddressTheChild(t *testing.T) {
	for name, run := range map[string]func(*fake, tool.Tool) error{
		Status: func(f *fake, candidate tool.Tool) error {
			_, err := candidate.Execute(context.Background(), bindingFor(f),
				`{"session":"main/seat_1-1"}`)
			return err
		},
		Answer: func(f *fake, candidate tool.Tool) error {
			_, err := candidate.Execute(context.Background(), bindingFor(f),
				`{"session":"main/seat_1-1","answer":"yes"}`)
			return err
		},
		Cancel: func(f *fake, candidate tool.Tool) error {
			_, err := candidate.Execute(context.Background(), bindingFor(f),
				`{"session":"main/seat_1-1"}`)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			capability := &fake{outcome: platform.DelegationOutcome{Status: "working"}}
			var candidate tool.Tool
			for _, offered := range (&Tool{}).Tools() {
				if offered.Definition().Name == name {
					candidate = offered
				}
			}
			if candidate == nil {
				t.Fatalf("tool %q is not registered", name)
			}
			if err := run(capability, candidate); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if capability.addressed != "main/seat_1-1" {
				t.Fatalf("addressed %q", capability.addressed)
			}
			if name == Answer && capability.answered.Text != "yes" {
				t.Fatalf("answer = %q", capability.answered.Text)
			}
		})
	}
}

// The log line an async hand-off leaves is built from String, so what it renders is worth pinning: the
// edge, its state and its depth, and never the payload.
func TestStringRendersTheEdgeAndNotThePayload(t *testing.T) {
	got := String(platform.DelegationOutcome{
		Status:  "submitted",
		Session: "main/seat_2-1",
		Depth:   2,
		Content: message.NewTextContent("a secret"),
	})
	if !strings.Contains(got, "main/seat_2-1") || !strings.Contains(got, "submitted") ||
		!strings.Contains(got, "depth 2") {
		t.Fatalf("String = %q, want the edge, its state and its depth", got)
	}
	if strings.Contains(got, "secret") {
		t.Fatalf("String = %q, want no payload", got)
	}
}
