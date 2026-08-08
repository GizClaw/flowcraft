package config

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	sdkeventconfig "github.com/GizClaw/flowcraft/sdk/event/config"
	"github.com/GizClaw/flowcraft/sdkx/delegation"
	"github.com/GizClaw/flowcraft/sdkx/delegation/kanban"
)

func TestMemoryDeployFactorySpec(t *testing.T) {
	want := sdkconfig.Spec{
		Kind: ResourceKind,
		Impl: "kanban-memory",
		Deps: []sdkconfig.DepSpec{{
			Name: EventBusDep,
			Type: sdkeventconfig.ResourceKind,
		}},
	}
	if got := NewMemoryDeployFactory().Spec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestMemoryDeployFactoryRejectsInvalidSettings(t *testing.T) {
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
	factory := NewMemoryDeployFactory()
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := factory.New(context.Background(), sdkconfig.Input{
				Settings: settingsJSON(t, input),
			})
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("New() error = %v, want validation error", err)
			}
		})
	}
}

func TestMemoryDeployFactoryAppliesSettings(t *testing.T) {
	value, err := NewMemoryDeployFactory().New(context.Background(), sdkconfig.Input{
		Settings: settingsJSON(t, `{
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
	if err := board.Complete(context.Background(), work.ID, work.LeaseToken, sdkdelegation.Response{
		ID:     work.ID,
		Status: sdkdelegation.StatusSucceeded,
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

func TestMemoryDeployFactoryAppliesMaxCards(t *testing.T) {
	value, err := NewMemoryDeployFactory().New(context.Background(), sdkconfig.Input{
		Settings: settingsJSON(t, `{"max_cards":1}`),
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
	if err := board.Complete(context.Background(), work.ID, work.LeaseToken, sdkdelegation.Response{
		ID:     work.ID,
		Status: sdkdelegation.StatusSucceeded,
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

func TestMemoryDeployFactoryAcceptsOmittedAndZeroSettings(t *testing.T) {
	for name, input := range map[string]string{
		"omitted": "",
		"zero":    `{"max_pending":0,"max_cards":0,"card_ttl":"0s"}`,
	} {
		t.Run(name, func(t *testing.T) {
			value, err := NewMemoryDeployFactory().New(context.Background(), sdkconfig.Input{
				Settings: settingsJSON(t, input),
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

func TestMemoryDeployFactoryBusOwnershipAndTypeMismatch(t *testing.T) {
	t.Run("owned bus", func(t *testing.T) {
		value, err := NewMemoryDeployFactory().New(context.Background(), sdkconfig.Input{})
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
		value, err := NewMemoryDeployFactory().New(context.Background(), sdkconfig.Input{
			Deps: map[string]any{EventBusDep: bus},
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
		_, err := NewMemoryDeployFactory().New(context.Background(), sdkconfig.Input{
			Deps: map[string]any{EventBusDep: "not a bus"},
		})
		if err == nil || !errdefs.IsValidation(err) {
			t.Fatalf("New() error = %v, want validation error", err)
		}
	})
}

func asyncRequest(input string) delegation.AsyncRequest {
	return delegation.AsyncRequest{
		Request: sdkdelegation.Request{
			Mode:   sdkdelegation.ModeAsync,
			Target: "worker",
			Input:  input,
		},
	}
}

func settingsJSON(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	if raw == "" {
		return nil
	}
	var out json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return out
}
