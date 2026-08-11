package inference

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPublicOperationsAreRecognized(t *testing.T) {
	for _, operation := range []Operation{
		OperationGenerate,
		OperationEmbed,
		OperationTranscription,
		OperationRealtime,
	} {
		if err := operation.Validate(); err != nil {
			t.Fatalf("%s Validate: %v", operation, err)
		}
	}
}

func TestModelRefSeparatesIdentityFromCredentialProfile(t *testing.T) {
	ref := ModelRef{
		ID:      ModelID{Provider: "openai", Name: "gpt-5"},
		Profile: "tenant-secret-profile",
	}
	if err := ref.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	metadata := (CompileReport{Operation: OperationGenerate}).Metadata(ref)
	if metadata.Model != ref.ID {
		t.Fatalf("metadata model = %+v, want %+v", metadata.Model, ref.ID)
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), ref.Profile) {
		t.Fatalf("metadata leaked credential profile: %s", data)
	}
}

func TestModelDescriptorValidatesDiscoveryMetadata(t *testing.T) {
	retirement := time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)
	descriptor := ModelDescriptor{
		ID:         ModelID{Provider: "openai", Name: "gpt-5"},
		Label:      "GPT-5",
		Operations: []Operation{OperationGenerate, OperationEmbed},
		Lifecycle: ModelLifecycle{
			Status:      ModelStatusDeprecated,
			RetiresAt:   &retirement,
			Replacement: &ModelID{Provider: "openai", Name: "gpt-6"},
		},
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	descriptor.Operations = append(descriptor.Operations, OperationGenerate)
	if err := descriptor.Validate(); err == nil {
		t.Fatal("expected duplicate operation to be rejected")
	}
}

func TestModelDescriptorAllowsEmptyProfileProjection(t *testing.T) {
	descriptor := ModelDescriptor{
		ID: ModelID{Provider: "openai", Name: "gpt-5"},
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("Validate empty operation projection: %v", err)
	}
}

func TestModelDescriptorCapabilitiesRequireGenerateOperation(t *testing.T) {
	descriptor := ModelDescriptor{
		ID:           ModelID{Provider: "openai", Name: "gpt-5"},
		Operations:   []Operation{OperationEmbed},
		Capabilities: ModelCapabilities{HostedWebSearch: true},
	}
	if err := descriptor.Validate(); err == nil {
		t.Fatal("Validate accepted hosted web search without the generate operation")
	}
	descriptor.Operations = []Operation{OperationGenerate}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("Validate with generate: %v", err)
	}
}

func TestModelDescriptorCloneCopiesCapabilities(t *testing.T) {
	descriptor := ModelDescriptor{
		ID:           ModelID{Provider: "openai", Name: "gpt-5"},
		Operations:   []Operation{OperationGenerate},
		Capabilities: ModelCapabilities{HostedWebSearch: true},
	}
	clone := descriptor.Clone()
	clone.Capabilities.HostedWebSearch = false
	if !descriptor.Capabilities.HostedWebSearch {
		t.Fatal("clone mutation leaked into the source descriptor")
	}
}

func TestModelLifecycleRejectsActiveRetirementMetadata(t *testing.T) {
	retirement := time.Now().UTC()
	descriptor := ModelDescriptor{
		ID:         ModelID{Provider: "openai", Name: "gpt-5"},
		Operations: []Operation{OperationGenerate},
		Lifecycle: ModelLifecycle{
			Status:    ModelStatusActive,
			RetiresAt: &retirement,
		},
	}
	if err := descriptor.Validate(); err == nil {
		t.Fatal("expected active lifecycle with retirement metadata to be rejected")
	}
}
