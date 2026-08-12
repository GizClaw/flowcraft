package resource_test

import (
	"context"
	"errors"
	"testing"
	"time"

	coredelegation "github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/delegation/kanban"
	kanbanresource "github.com/GizClaw/flowcraft/core/delegation/kanban/resource"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestMemoryFactorySpec(t *testing.T) {
	spec := kanbanresource.NewMemoryFactory().Spec()
	if spec.Kind != kanbanresource.ResourceKind || spec.Impl != "kanban-memory" {
		t.Fatalf("Spec() = %+v", spec)
	}
	if len(spec.Deps) != 1 ||
		spec.Deps[0].Name != kanbanresource.EventBusDep ||
		spec.Deps[0].Type != "event.Bus" {
		t.Fatalf("deps = %+v", spec.Deps)
	}
}

func TestMemoryFactoryRejectsInvalidSettings(t *testing.T) {
	tests := map[string]string{
		"unknown field":      `{"unknown":true}`,
		"empty scope":        `{"scope_id":""}`,
		"blank scope":        `{"scope_id":"  "}`,
		"negative pending":   `{"max_pending":-1}`,
		"negative cards":     `{"max_cards":-1}`,
		"empty card ttl":     `{"card_ttl":""}`,
		"negative card ttl":  `{"card_ttl":"-1s"}`,
		"malformed card ttl": `{"card_ttl":"eventually"}`,
	}
	factory := kanbanresource.NewMemoryFactory()
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := factory.New(context.Background(), resource.Input{
				Settings: []byte(input),
			})
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("New() error = %v, want validation error", err)
			}
		})
	}
}

func TestMemoryFactoryAppliesSettings(t *testing.T) {
	value, err := kanbanresource.NewMemoryFactory().New(context.Background(), resource.Input{
		Settings: []byte(`{
			"scope_id": "jobs",
			"max_pending": 1,
			"max_cards": 10,
			"card_ttl": "1ns"
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	board := value.(*kanban.Board)
	t.Cleanup(func() { _ = board.Close() })

	if got := board.ScopeID(); got != "jobs" {
		t.Fatalf("ScopeID() = %q, want jobs", got)
	}
	first, err := board.Submit(context.Background(), asyncRequest("first"))
	if err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	if _, err := board.Submit(context.Background(), asyncRequest("blocked")); !errdefs.IsRateLimit(err) {
		t.Fatalf("second Submit error = %v, want rate limit", err)
	}
	work, err := board.Claim(context.Background())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := board.Complete(context.Background(), work.ID, work.LeaseToken, coredelegation.Response{
		ID:     work.ID,
		Status: coredelegation.StatusSucceeded,
		Output: "done",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	time.Sleep(time.Millisecond)
	if _, err := board.Submit(context.Background(), asyncRequest("second")); err != nil {
		t.Fatalf("Submit after completion: %v", err)
	}
	if _, ok := board.Card(first); ok {
		t.Fatal("expired terminal card was retained")
	}
}

func TestMemoryFactoryAppliesMaxCards(t *testing.T) {
	value, err := kanbanresource.NewMemoryFactory().New(context.Background(), resource.Input{
		Settings: []byte(`{"max_cards":1}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	board := value.(*kanban.Board)
	t.Cleanup(func() { _ = board.Close() })

	first, err := board.Submit(context.Background(), asyncRequest("first"))
	if err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	work, err := board.Claim(context.Background())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := board.Complete(context.Background(), work.ID, work.LeaseToken, coredelegation.Response{
		ID:     work.ID,
		Status: coredelegation.StatusSucceeded,
		Output: "done",
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := board.Submit(context.Background(), asyncRequest("second")); err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if _, ok := board.Card(first); ok {
		t.Fatal("oldest terminal card was retained above max_cards")
	}
	if got := board.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
}

func TestMemoryFactoryAcceptsOmittedAndZeroSettings(t *testing.T) {
	for name, input := range map[string]string{
		"omitted": "",
		"zero":    `{"max_pending":0,"max_cards":0,"card_ttl":"0s"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var settings []byte
			if input != "" {
				settings = []byte(input)
			}
			value, err := kanbanresource.NewMemoryFactory().New(context.Background(), resource.Input{
				Settings: settings,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := value.(*kanban.Board).Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}
}

func TestMemoryFactoryBusOwnershipAndTypeMismatch(t *testing.T) {
	t.Run("owned bus", func(t *testing.T) {
		value, err := kanbanresource.NewMemoryFactory().New(context.Background(), resource.Input{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		board := value.(*kanban.Board)
		if err := board.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := board.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if err := board.Bus().Publish(context.Background(), event.Envelope{}); !errors.Is(err, event.ErrBusClosed) {
			t.Fatalf("owned bus Publish after Close = %v, want ErrBusClosed", err)
		}
	})

	t.Run("shared bus", func(t *testing.T) {
		bus := event.NewMemoryBus()
		t.Cleanup(func() { _ = bus.Close() })
		value, err := kanbanresource.NewMemoryFactory().New(context.Background(), resource.Input{
			Deps: map[string]any{kanbanresource.EventBusDep: bus},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		board := value.(*kanban.Board)
		if err := board.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := bus.Publish(context.Background(), event.Envelope{}); err != nil {
			t.Fatalf("shared bus was closed by backend: %v", err)
		}
	})

	t.Run("type mismatch", func(t *testing.T) {
		_, err := kanbanresource.NewMemoryFactory().New(context.Background(), resource.Input{
			Deps: map[string]any{kanbanresource.EventBusDep: "not a bus"},
		})
		if err == nil || !errdefs.IsValidation(err) {
			t.Fatalf("New() error = %v, want validation error", err)
		}
	})
}

func asyncRequest(input string) coredelegation.AsyncRequest {
	return coredelegation.AsyncRequest{
		Request: coredelegation.Request{
			Mode:   coredelegation.ModeAsync,
			Target: "worker",
			Input:  input,
		},
	}
}
