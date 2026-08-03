package config

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdkx/delegation"
	"github.com/GizClaw/flowcraft/sdkx/delegation/kanban"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	eventconfig "github.com/GizClaw/flowcraft/sdkx/event/config"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestMemoryDeployFactorySpec(t *testing.T) {
	want := deploy.ResourceSpec{
		Kind: ResourceKind,
		Impl: "kanban-memory",
		Deps: []deploy.ResourceDepSpec{{
			Name: EventBusDep,
			Type: eventconfig.ResourceKind,
		}},
	}
	if got := NewMemoryDeployFactory().Spec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestMemoryDeployFactoryRejectsInvalidSettings(t *testing.T) {
	tests := map[string]string{
		"unknown field":      "unknown: true\n",
		"empty scope":        "scope_id: \"\"\n",
		"blank scope":        "scope_id: \"  \"\n",
		"negative pending":   "max_pending: -1\n",
		"negative cards":     "max_cards: -1\n",
		"empty card ttl":     "card_ttl: \"\"\n",
		"negative card ttl":  "card_ttl: -1s\n",
		"malformed card ttl": "card_ttl: eventually\n",
	}
	factory := NewMemoryDeployFactory()
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := factory.New(context.Background(), deploy.ResourceInput{
				Settings: settingsNode(t, input),
			})
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("New() error = %v, want validation error", err)
			}
		})
	}
}

func TestMemoryDeployFactoryAppliesSettings(t *testing.T) {
	value, err := NewMemoryDeployFactory().New(context.Background(), deploy.ResourceInput{
		Settings: settingsNode(t, `
scope_id: jobs
max_pending: 1
max_cards: 10
card_ttl: 1ns
`),
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
	value, err := NewMemoryDeployFactory().New(context.Background(), deploy.ResourceInput{
		Settings: settingsNode(t, "max_cards: 1\n"),
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
		"zero":    "max_pending: 0\nmax_cards: 0\ncard_ttl: 0s\n",
	} {
		t.Run(name, func(t *testing.T) {
			value, err := NewMemoryDeployFactory().New(context.Background(), deploy.ResourceInput{
				Settings: settingsNode(t, input),
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
		value, err := NewMemoryDeployFactory().New(context.Background(), deploy.ResourceInput{})
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
		value, err := NewMemoryDeployFactory().New(context.Background(), deploy.ResourceInput{
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
		_, err := NewMemoryDeployFactory().New(context.Background(), deploy.ResourceInput{
			Deps: map[string]any{EventBusDep: "not a bus"},
		})
		if err == nil || !errdefs.IsValidation(err) {
			t.Fatalf("New() error = %v, want validation error", err)
		}
	})
}

func TestMemoryDeployResourceExportsAndClosesInDependencyOrder(t *testing.T) {
	var board *kanban.Board
	bus := &checkingBus{
		Bus: event.NewMemoryBus(),
		onClose: func() {
			select {
			case <-board.Context().Done():
			default:
				t.Error("event bus closed before dependent kanban backend")
			}
		},
	}
	builder := deploy.NewBuilder(agent.NewRegistry())
	builder.MustRegisterResource(staticBusFactory{bus: bus})
	builder.MustRegisterResource(NewMemoryDeployFactory())

	doc, err := deploy.Parse([]byte(`
version: v1
resources:
  events:
    kind: event.Bus
    impl: checking
  delegations:
    kind: delegation.AsyncBackend
    impl: kanban-memory
    export: true
    deps:
      event_bus: events
    settings:
      scope_id: jobs
agents: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := deploy.ResourceAs[delegation.AsyncBackend](result, "delegations")
	if err != nil {
		t.Fatalf("ResourceAs[delegation.AsyncBackend]: %v", err)
	}
	board, err = deploy.ResourceAs[*kanban.Board](result, "delegations")
	if err != nil {
		t.Fatalf("ResourceAs[*kanban.Board]: %v", err)
	}
	if backend != board {
		t.Fatal("interface and concrete exports refer to different backends")
	}
	if _, err := deploy.ResourceAs[event.Bus](result, "delegations"); !errdefs.IsValidation(err) {
		t.Fatalf("ResourceAs[event.Bus] error = %v, want validation error", err)
	}

	if err := result.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := result.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := bus.Publish(context.Background(), event.Envelope{}); !errors.Is(err, event.ErrBusClosed) {
		t.Fatalf("dependency bus Publish after Result.Close = %v, want ErrBusClosed", err)
	}
}

type checkingBus struct {
	event.Bus
	onClose func()
}

func (b *checkingBus) Close() error {
	b.onClose()
	return b.Bus.Close()
}

type staticBusFactory struct {
	bus event.Bus
}

func (staticBusFactory) Spec() deploy.ResourceSpec {
	return deploy.ResourceSpec{Kind: eventconfig.ResourceKind, Impl: "checking"}
}

func (f staticBusFactory) New(context.Context, deploy.ResourceInput) (any, error) {
	return f.bus, nil
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

func settingsNode(t *testing.T, input string) *yamlv3.Node {
	t.Helper()
	if input == "" {
		return nil
	}
	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(input), &node); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return node.Content[0]
}
