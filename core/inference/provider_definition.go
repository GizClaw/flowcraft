package inference

import (
	"context"
	"fmt"
)

type (
	// OpenGenerate materializes unary and/or finite streaming Generate drivers.
	// Openers must not send inference requests, perform remote model discovery,
	// or otherwise depend on provider I/O. Runtime may share one opener across
	// callers and lets it finish after an individual caller is canceled, so the
	// context is runtime-owned rather than the context passed to an operation.
	// Openers must therefore be local, bounded construction work.
	OpenGenerate func(context.Context, ModelRef) (GenerateOperations, error)
	// OpenEmbed materializes an embedding driver. It follows OpenGenerate's
	// runtime-owned context and local, bounded construction contract.
	OpenEmbed func(context.Context, ModelRef) (EmbedDriver, error)
)

type Openers struct {
	Generate OpenGenerate
	Embed    OpenEmbed
}

// ModelImplementation binds descriptive metadata to the operation openers that
// actually implement the model. Descriptor.Operations is derived from Openers
// during Runtime construction.
type ModelImplementation struct {
	Descriptor ModelDescriptor
	Openers    Openers
}

// ProfileDefinition declares an exact credential-profile identifier and the
// operations its backing credentials and endpoints can execute. An empty
// Operations list leaves the profile unrestricted.
type ProfileDefinition struct {
	ID         string
	Operations []Operation
}

// ProviderDefinition is copied and frozen by NewRuntime. An empty Profiles
// list declares one unrestricted default profile ("").
type ProviderDefinition struct {
	ID       string
	Profiles []ProfileDefinition
	Models   []ModelImplementation
	Dynamic  *Openers
}

type profileEntry struct {
	// A nil operation set means the profile is unrestricted.
	operations map[Operation]struct{}
}

type providerEntry struct {
	definition ProviderDefinition
	profiles   map[string]profileEntry
	models     map[string]int
}

func buildRegistry(
	definitions []ProviderDefinition,
) (map[string]providerEntry, error) {
	if len(definitions) == 0 {
		return nil, fmt.Errorf("inference runtime requires at least one provider")
	}
	registry := make(map[string]providerEntry, len(definitions))
	for index, definition := range definitions {
		if !extensionIDPattern.MatchString(definition.ID) {
			return nil, fmt.Errorf("provider %d has invalid identity %q", index, definition.ID)
		}
		if _, ok := registry[definition.ID]; ok {
			return nil, fmt.Errorf("duplicate provider %q", definition.ID)
		}
		provider, err := freezeProviderDefinition(definition)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", definition.ID, err)
		}
		registry[definition.ID] = provider
	}
	return registry, nil
}

func freezeProviderDefinition(
	definition ProviderDefinition,
) (providerEntry, error) {
	profiles := definition.Profiles
	if len(profiles) == 0 {
		profiles = []ProfileDefinition{{}}
	}
	frozen := providerEntry{
		definition: ProviderDefinition{
			ID:     definition.ID,
			Models: make([]ModelImplementation, 0, len(definition.Models)),
		},
		profiles: make(map[string]profileEntry, len(profiles)),
		models:   make(map[string]int, len(definition.Models)),
	}
	for _, profile := range profiles {
		if profile.ID != "" && !extensionIDPattern.MatchString(profile.ID) {
			return providerEntry{}, fmt.Errorf("invalid profile %q", profile.ID)
		}
		if _, ok := frozen.profiles[profile.ID]; ok {
			return providerEntry{}, fmt.Errorf("duplicate profile %q", profile.ID)
		}
		var entry profileEntry
		if len(profile.Operations) > 0 {
			entry.operations = make(
				map[Operation]struct{},
				len(profile.Operations),
			)
		}
		for _, operation := range profile.Operations {
			if err := operation.Validate(); err != nil {
				return providerEntry{}, fmt.Errorf(
					"profile %q: %w",
					profile.ID,
					err,
				)
			}
			if _, ok := entry.operations[operation]; ok {
				return providerEntry{}, fmt.Errorf(
					"profile %q has duplicate operation %q",
					profile.ID,
					operation,
				)
			}
			entry.operations[operation] = struct{}{}
		}
		frozen.profiles[profile.ID] = entry
	}
	for _, definition := range definition.Models {
		model, err := freezeModelDefinition(frozen.definition.ID, definition)
		if err != nil {
			return providerEntry{}, err
		}
		if _, ok := frozen.models[model.Descriptor.ID.Name]; ok {
			return providerEntry{}, fmt.Errorf(
				"duplicate model %q",
				model.Descriptor.ID.Name,
			)
		}
		frozen.models[model.Descriptor.ID.Name] = len(frozen.definition.Models)
		frozen.definition.Models = append(frozen.definition.Models, model)
	}
	if definition.Dynamic != nil {
		dynamic := *definition.Dynamic
		if len(dynamic.Operations()) == 0 {
			return providerEntry{}, fmt.Errorf(
				"dynamic model definition has no operations",
			)
		}
		frozen.definition.Dynamic = &dynamic
	}
	if len(frozen.models) == 0 && frozen.definition.Dynamic == nil {
		return providerEntry{}, fmt.Errorf("provider has no models")
	}
	return frozen, nil
}

func freezeModelDefinition(
	provider string,
	definition ModelImplementation,
) (ModelImplementation, error) {
	descriptor := definition.Descriptor.Clone()
	if descriptor.ID.Provider != provider {
		return ModelImplementation{}, fmt.Errorf(
			"model %q belongs to provider %q",
			descriptor.ID.Name,
			descriptor.ID.Provider,
		)
	}
	descriptor.Operations = definition.Openers.Operations()
	if len(descriptor.Operations) == 0 {
		return ModelImplementation{}, fmt.Errorf("model must expose at least one operation")
	}
	if err := descriptor.Validate(); err != nil {
		return ModelImplementation{}, err
	}
	return ModelImplementation{
		Descriptor: descriptor,
		Openers:    definition.Openers,
	}, nil
}

func (profile profileEntry) allows(operation Operation) bool {
	if profile.operations == nil {
		return true
	}
	_, ok := profile.operations[operation]
	return ok
}

func (profile profileEntry) filter(operations []Operation) []Operation {
	if profile.operations == nil {
		return append([]Operation(nil), operations...)
	}
	filtered := make([]Operation, 0, len(operations))
	for _, operation := range operations {
		if profile.allows(operation) {
			filtered = append(filtered, operation)
		}
	}
	return filtered
}

func (openers Openers) Operations() []Operation {
	var operations []Operation
	if openers.Generate != nil {
		operations = append(operations, OperationGenerate)
	}
	if openers.Embed != nil {
		operations = append(operations, OperationEmbed)
	}
	return operations
}
