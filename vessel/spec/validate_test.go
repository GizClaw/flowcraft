package spec

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestValidate_OK(t *testing.T) {
	t.Parallel()
	probe := ProbeFunc{Label: "fake", Fn: func(context.Context) (ProbeResult, error) {
		return ProbeResult{Healthy: true}, nil
	}}
	s := Spec{
		ID: "v-1",
		Agents: []Agent{
			{Name: "primary"},
			{Name: "moderator", HistoryAccess: HistoryAccessReadOnly, Sidecar: true, SubscribeTo: "agent.run.>"},
		},
		History: &History{Kind: "buffer", MaxMessages: 50},
		Resources: Resources{
			MaxConcurrentRuns: 4,
			TurnTimeout:       30 * time.Second,
		},
		Restart: Restart{Mode: RestartOnFailure, MaxRestarts: 3, BackoffInit: time.Second, BackoffMax: 30 * time.Second},
		Probes:  &Probes{Liveness: []Probe{probe}, Interval: time.Minute, Timeout: time.Second, FailureThreshold: 3},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_RejectsEmptyAgents(t *testing.T) {
	t.Parallel()
	if err := (Spec{}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_RejectsEmptyAgentName(t *testing.T) {
	t.Parallel()
	if err := (Spec{Agents: []Agent{{Name: ""}}}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_RejectsDuplicateAgentName(t *testing.T) {
	t.Parallel()
	if err := (Spec{Agents: []Agent{{Name: "a"}, {Name: "a"}}}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_RejectsInvalidHistoryAccess(t *testing.T) {
	t.Parallel()
	s := Spec{Agents: []Agent{{Name: "a", HistoryAccess: "wat"}}}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_RequiresSubscribeForSidecar(t *testing.T) {
	t.Parallel()
	s := Spec{Agents: []Agent{{Name: "a", Sidecar: true}}}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_RejectsSubscribeWithoutSidecar(t *testing.T) {
	t.Parallel()
	s := Spec{Agents: []Agent{{Name: "a", SubscribeTo: "x.>"}}}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_RejectsInvalidHistoryKind(t *testing.T) {
	t.Parallel()
	s := Spec{
		Agents:  []Agent{{Name: "a"}},
		History: &History{Kind: "wat"},
	}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_RejectsNegativeResources(t *testing.T) {
	t.Parallel()
	s := Spec{
		Agents:    []Agent{{Name: "a"}},
		Resources: Resources{MaxConcurrentRuns: -1},
	}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_RejectsRestartAlways(t *testing.T) {
	t.Parallel()
	s := Spec{
		Agents:  []Agent{{Name: "a"}},
		Restart: Restart{Mode: "always"},
	}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error rejecting unsupported mode, got %v", err)
	}
}

func TestValidate_RejectsNilProbe(t *testing.T) {
	t.Parallel()
	s := Spec{
		Agents: []Agent{{Name: "a"}},
		Probes: &Probes{Liveness: []Probe{nil}},
	}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_RejectsUnnamedProbe(t *testing.T) {
	t.Parallel()
	s := Spec{
		Agents: []Agent{{Name: "a"}},
		Probes: &Probes{Liveness: []Probe{ProbeFunc{Label: ""}}},
	}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_DispatcherRequiresKanban(t *testing.T) {
	t.Parallel()
	s := Spec{Agents: []Agent{{Name: "boss", Dispatcher: true}}}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_DispatcherWithKanbanOK(t *testing.T) {
	t.Parallel()
	s := Spec{
		Agents: []Agent{
			{Name: "boss", Dispatcher: true, ProducerChain: 5},
			{Name: "worker"},
		},
		Kanban: &Kanban{MaxPendingTasks: 10, MaxProducerChain: 4, CallbackMaxSummary: 256},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_RejectsNegativeProducerChain(t *testing.T) {
	t.Parallel()
	s := Spec{
		Agents: []Agent{{Name: "boss", Dispatcher: true, ProducerChain: -1}},
		Kanban: &Kanban{},
	}
	if err := s.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation error, got %v", err)
	}
}

func TestValidate_RejectsNegativeKanbanFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		k    Kanban
	}{
		{"MaxPendingTasks", Kanban{MaxPendingTasks: -1}},
		{"MaxProducerChain", Kanban{MaxProducerChain: -1}},
		{"CallbackMaxSummary", Kanban{CallbackMaxSummary: -1}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := Spec{Agents: []Agent{{Name: "a"}}, Kanban: &tc.k}
			if err := s.Validate(); !errdefs.IsValidation(err) {
				t.Fatalf("expected Validation error, got %v", err)
			}
		})
	}
}

func TestAgent_Lookup(t *testing.T) {
	t.Parallel()
	s := Spec{Agents: []Agent{{Name: "first"}, {Name: "second"}}}
	got, ok := s.Agent("second")
	if !ok || got == nil || got.Name != "second" {
		t.Fatalf("Agent(second) → %v, %v", got, ok)
	}
	if _, ok := s.Agent("missing"); ok {
		t.Fatal("Agent(missing) → ok=true, want false")
	}
}
