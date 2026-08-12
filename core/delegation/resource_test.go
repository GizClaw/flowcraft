package delegation_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestServiceFactoryRequiresDirectory(t *testing.T) {
	_, err := (delegation.NewServiceFactory(nil)).New(context.Background(), resource.Input{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New without directory = %v, want Validation", err)
	}
}

func TestServiceFactoryAppliesSettings(t *testing.T) {
	factory := delegation.NewServiceFactory(delegation.NewDirectory())
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{
			"max_concurrency": 2,
			"max_depth": 3,
			"timeout": "5s",
			"idempotency_retention": "30m",
			"defer_workers": true
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	service, ok := value.(*delegation.LocalService)
	if !ok {
		t.Fatalf("New returned %T, want *delegation.LocalService", value)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestServiceFactoryRejectsBadSettings(t *testing.T) {
	factory := delegation.NewServiceFactory(delegation.NewDirectory())
	for name, input := range map[string]string{
		"negative concurrency": `{"max_concurrency":0}`,
		"negative depth":       `{"max_depth":0}`,
		"bad timeout":          `{"timeout":"eventually"}`,
		"bad retention":        `{"idempotency_retention":"eventually"}`,
		"unknown field":        `{"unknown":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := factory.New(context.Background(), resource.Input{
				Settings: []byte(input),
			})
			if !errdefs.IsValidation(err) {
				t.Fatalf("New = %v, want Validation", err)
			}
		})
	}
}

func TestServiceFactoryRejectsWrongBackend(t *testing.T) {
	factory := delegation.NewServiceFactory(delegation.NewDirectory())
	_, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{delegation.BackendDep: "not a backend"},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New with wrong backend = %v, want Validation", err)
	}
}
