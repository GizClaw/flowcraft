package inferencetest

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// CatalogFixture describes a provider that publishes the deployment's model
// declarations.
type CatalogFixture struct {
	// ProviderID is the driver's identity; every published model must carry it.
	ProviderID string
	// NewProvider builds a provider pointed at baseURL. The suite passes an
	// unreachable address when it only exercises declaration checks, so a driver
	// that fails to refuse a request shows up as a transport error rather than a
	// network call.
	NewGenerator func(t *testing.T, baseURL string) (inference.Provider, error)
	// Declared is the line-up the fixture expects to be published.
	Declared []model.ModelDescriptor
	// Served lists the output modalities the driver actually produces, for the
	// coverage check.
	Served []inference.Output
	// Request builds a request for one model and output.
	Request func(t *testing.T, modelName string, output inference.Output) inference.GenerateRequest
	// Enforces says the driver refuses requests that the declarations exclude.
	Enforces bool
	// UndeclaredInput builds a request carrying an input kind that its own
	// declarations exclude, and names that kind. The fixture chooses a model it
	// declared, so the suite needs no knowledge of the line-up.
	UndeclaredInput func(t *testing.T) (inference.GenerateRequest, message.PartKind)
}

// RunCatalog runs the declaration suites: publication, lookup, defensive copies,
// coverage and enforcement.
func RunCatalog(t *testing.T, fixture CatalogFixture) {
	t.Helper()
	if fixture.NewGenerator == nil {
		t.Fatalf("inferencetest: catalog fixture needs NewProvider")
	}
	// No I/O at construction, so an unreachable endpoint proves enforcement
	// without touching a network.
	const unreachable = "http://127.0.0.1:1"

	t.Run("declarations are published", func(t *testing.T) {
		provider, err := fixture.NewGenerator(t, unreachable)
		if err != nil {
			t.Fatalf("new provider: %v", err)
		}
		catalog := provider.Catalog
		if catalog == nil {
			t.Fatal("provider does not publish a catalog")
		}
		published := catalog.Models()
		if len(published) == 0 {
			t.Fatal("catalog is empty")
		}
		seen := make(map[model.ModelID]struct{}, len(published))
		for _, descriptor := range published {
			if err := descriptor.Validate(); err != nil {
				t.Fatalf("model %q is invalid: %v", descriptor.ID.Name, err)
			}
			if descriptor.ID.Provider != fixture.ProviderID {
				t.Errorf("model %q declares provider %q, want %q",
					descriptor.ID.Name, descriptor.ID.Provider, fixture.ProviderID)
			}
			if _, ok := seen[descriptor.ID]; ok {
				t.Errorf("model %q is declared twice", descriptor.ID.Name)
			}
			seen[descriptor.ID] = struct{}{}
		}
		if len(published) != len(fixture.Declared) {
			t.Errorf("published %d models, fixture declares %d", len(published), len(fixture.Declared))
		}
	})

	t.Run("lookup agrees with the list", func(t *testing.T) {
		provider, err := fixture.NewGenerator(t, unreachable)
		if err != nil {
			t.Fatalf("new provider: %v", err)
		}
		catalog := provider.Catalog
		for _, descriptor := range catalog.Models() {
			described, ok := catalog.Describe(descriptor.ID)
			if !ok {
				t.Errorf("model %q is listed but not described", descriptor.ID.Name)
				continue
			}
			if described.ID != descriptor.ID || described.Label != descriptor.Label {
				t.Errorf("describe(%q) = %+v, listed %+v", descriptor.ID.Name, described, descriptor)
			}
		}
		unknown := model.ModelID{Provider: fixture.ProviderID, Name: "definitely-not-declared"}
		if _, ok := catalog.Describe(unknown); ok {
			t.Error("an undeclared model was described")
		}
	})

	// Callers may sort, filter or annotate what they receive; the catalog itself
	// must not change as a result.
	t.Run("published declarations are copies", func(t *testing.T) {
		provider, err := fixture.NewGenerator(t, unreachable)
		if err != nil {
			t.Fatalf("new provider: %v", err)
		}
		catalog := provider.Catalog
		first := catalog.Models()
		if len(first) == 0 {
			t.Fatal("catalog is empty")
		}
		original := first[0].Label
		first[0].Label = "mutated by the caller"
		first[0].Capabilities.Inputs = append(first[0].Capabilities.Inputs, "injected")
		second := catalog.Models()
		if second[0].Label != original {
			t.Fatalf("mutating the returned slice changed the catalog: %q", second[0].Label)
		}
		for _, kind := range second[0].Capabilities.Inputs {
			if string(kind) == "injected" {
				t.Fatal("mutating the returned capabilities changed the catalog")
			}
		}

		described, _ := catalog.Describe(second[0].ID)
		described.Label = "mutated by the caller"
		again, _ := catalog.Describe(second[0].ID)
		if again.Label != original {
			t.Fatalf("mutating a described model changed the catalog: %q", again.Label)
		}
	})

	t.Run("declarations cover what the driver serves", func(t *testing.T) {
		for _, output := range fixture.Served {
			want := output.PartKind()
			covered := false
			for _, descriptor := range fixture.Declared {
				if len(descriptor.Capabilities.Outputs) == 0 {
					// An empty list declares nothing and filters nothing.
					continue
				}
				for _, declared := range descriptor.Capabilities.Outputs {
					if declared == want {
						covered = true
					}
				}
			}
			if !covered {
				t.Errorf("no declared model covers output %q", output)
			}
		}
	})

	if !fixture.Enforces {
		return
	}

	t.Run("an undeclared model is refused", func(t *testing.T) {
		provider, err := fixture.NewGenerator(t, unreachable)
		if err != nil {
			t.Fatalf("new provider: %v", err)
		}
		_, err = provider.Generate(
			context.Background(),
			fixture.Request(t, "definitely-not-declared", inference.OutputText),
		)
		if !inference.IsKind(err, inference.UnknownModel) {
			t.Fatalf("kind = %v, want UnknownModel", err)
		}
	})

	t.Run("an undeclared input is refused", func(t *testing.T) {
		if fixture.UndeclaredInput == nil {
			t.Skip("the fixture declares no input restriction")
		}
		request, kind := fixture.UndeclaredInput(t)
		provider, err := fixture.NewGenerator(t, unreachable)
		if err != nil {
			t.Fatalf("new provider: %v", err)
		}
		_, err = provider.Generate(context.Background(), request)
		if !inference.IsKind(err, inference.UnsupportedFeature) {
			t.Fatalf("kind = %v, want UnsupportedFeature for %q input", err, kind)
		}
	})

	t.Run("an undeclared output is refused", func(t *testing.T) {
		// Pick a model that declares outputs, then ask for one it excludes.
		var target model.ModelDescriptor
		var missing inference.Output
		for _, descriptor := range fixture.Declared {
			if len(descriptor.Capabilities.Outputs) == 0 {
				continue
			}
			for _, output := range fixture.Served {
				if !declaresOutput(descriptor, output) {
					target, missing = descriptor, output
					break
				}
			}
			if missing != "" {
				break
			}
		}
		if missing == "" {
			t.Skip("the fixture declares no model that excludes a served output")
		}
		provider, err := fixture.NewGenerator(t, unreachable)
		if err != nil {
			t.Fatalf("new provider: %v", err)
		}
		_, err = provider.Generate(context.Background(), fixture.Request(t, target.ID.Name, missing))
		if !inference.IsKind(err, inference.UnsupportedOperation) {
			t.Fatalf("kind = %v, want UnsupportedOperation", err)
		}
	})
}

func declaresOutput(descriptor model.ModelDescriptor, output inference.Output) bool {
	want := output.PartKind()
	for _, declared := range descriptor.Capabilities.Outputs {
		if declared == want {
			return true
		}
	}
	return false
}
