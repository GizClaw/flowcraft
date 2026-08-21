package delegation_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestServiceFactoryRequiresDirectory(t *testing.T) {
	_, err := (delegation.NewServiceFactory()).New(context.Background(), resource.Input{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New without directory = %v, want Validation", err)
	}
}

func TestServiceFactoryAppliesSettings(t *testing.T) {
	factory := delegation.NewServiceFactory()
	value, err := factory.New(context.Background(), resource.Input{
		Settings: []byte(`{
			"max_concurrency": 2,
			"max_depth": 3,
			"timeout": "5s",
			"idempotency_retention": "30m",
			"defer_workers": true
		}`),
		Deps: map[string]any{
			delegation.DirectoryDep: delegation.NewDirectory(),
		},
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
	factory := delegation.NewServiceFactory()
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
	factory := delegation.NewServiceFactory()
	_, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{
			delegation.DirectoryDep: delegation.NewDirectory(),
			delegation.BackendDep:   "not a backend",
		},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New with wrong backend = %v, want Validation", err)
	}
}

func TestServiceFactoryRejectsWrongDirectory(t *testing.T) {
	factory := delegation.NewServiceFactory()
	_, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{delegation.DirectoryDep: "not a directory"},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New with wrong directory = %v, want Validation", err)
	}
}

func TestServiceFactoryAcceptsSessionProvider(t *testing.T) {
	factory := delegation.NewServiceFactory()
	value, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{
			delegation.DirectoryDep:       delegation.NewDirectory(),
			delegation.SessionProviderDep: delegation.RandomSessionProvider{},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	service := value.(*delegation.LocalService)
	if err := service.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestServiceFactoryRejectsWrongSessionProvider(t *testing.T) {
	factory := delegation.NewServiceFactory()
	_, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{
			delegation.DirectoryDep:       delegation.NewDirectory(),
			delegation.SessionProviderDep: "not a provider",
		},
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New with wrong provider = %v, want Validation", err)
	}
}

func TestDirectoryFactoryBuildsUnboundDirectory(t *testing.T) {
	factory := delegation.NewDirectoryFactory()
	value, err := factory.New(context.Background(), resource.Input{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	directory, ok := value.(*delegation.LocalDirectory)
	if !ok {
		t.Fatalf("New returned %T, want *delegation.LocalDirectory", value)
	}
	if _, err := directory.List(context.Background()); !errdefs.IsNotAvailable(err) {
		t.Fatalf("unbound directory List error = %v, want not available", err)
	}
}

func TestRandomSessionProviderFactory(t *testing.T) {
	factory := delegation.NewRandomSessionProviderFactory()
	value, err := factory.New(context.Background(), resource.Input{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider, ok := value.(delegation.SessionProvider)
	if !ok {
		t.Fatalf("New returned %T, want delegation.SessionProvider", value)
	}
	if provider.Persistent() {
		t.Fatal("random provider must not be persistent")
	}
	first, err := provider.CreateContextID(context.Background(), delegation.AsyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.CreateContextID(context.Background(), delegation.AsyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first == second {
		t.Fatalf("random context ids = %q %q, want unique non-empty", first, second)
	}
}
