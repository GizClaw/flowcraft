package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

func TestBuilderResolvesSecretsAndBuildsRuntimeExplicitly(t *testing.T) {
	var received ProviderInput
	builder, err := NewBuilder(
		map[string]Factory{
			"openai": FactoryFunc(func(
				_ context.Context,
				input ProviderInput,
			) (inference.ProviderDefinition, error) {
				received = input.Clone()
				return inference.ProviderDefinition{
					ID: input.ID,
					Profiles: []inference.ProfileDefinition{{
						ID: input.Profiles[0].ID,
						Operations: []inference.Operation{
							inference.OperationGenerate,
							inference.OperationEmbed,
						},
					}},
					Models: []inference.ModelImplementation{{
						Descriptor: inference.ModelDescriptor{
							ID: inference.ModelID{
								Provider: input.ID,
								Name:     "custom-model",
							},
						},
						Openers: inference.Openers{
							Generate: func(
								context.Context,
								inference.ModelRef,
							) (inference.GenerateOperations, error) {
								return inference.GenerateOperations{}, nil
							},
						},
					}},
				}, nil
			}),
		},
		map[string]SecretResolver{
			"env": SecretResolverFunc(func(
				_ context.Context,
				key string,
			) (Secret, error) {
				if key != "OPENAI_API_KEY" {
					t.Fatalf("secret key = %q", key)
				}
				return NewSecret([]byte("resolved-secret"))
			}),
		},
	)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	document := Document{
		Version: VersionV1,
		Providers: []ProviderConfig{{
			ID:     "openai",
			Driver: "openai",
			Spec:   json.RawMessage(`{"models":["custom-model"]}`),
			Profiles: []ProfileConfig{{
				ID:         "default",
				Operations: []inference.Operation{inference.OperationGenerate},
				Secrets: map[string]SecretRef{
					"api_key": {Resolver: "env", Key: "OPENAI_API_KEY"},
				},
			}},
		}},
	}
	runtime, err := builder.NewRuntime(t.Context(), document)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	descriptor, err := runtime.InspectModel(inference.ModelRef{
		ID:      inference.ModelID{Provider: "openai", Name: "custom-model"},
		Profile: "default",
	})
	if err != nil {
		t.Fatalf("InspectModel: %v", err)
	}
	if len(descriptor.Operations) != 1 ||
		descriptor.Operations[0] != inference.OperationGenerate {
		t.Fatalf("operations = %v", descriptor.Operations)
	}
	if string(received.Profiles[0].Secrets["api_key"].Bytes()) !=
		"resolved-secret" ||
		string(received.Spec) != `{"models":["custom-model"]}` {
		t.Fatalf("factory input = %+v", received)
	}
}

func TestBuilderRejectsMissingFactoryAndResolver(t *testing.T) {
	document := Document{
		Version: VersionV1,
		Providers: []ProviderConfig{{
			ID:     "openai",
			Driver: "openai",
			Profiles: []ProfileConfig{{
				Secrets: map[string]SecretRef{
					"api_key": {Resolver: "env", Key: "OPENAI_API_KEY"},
				},
			}},
		}},
	}
	builder, err := NewBuilder(nil, nil)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := builder.Build(t.Context(), document); err == nil ||
		!strings.Contains(err.Error(), `factory "openai"`) {
		t.Fatalf("missing factory error = %v", err)
	}

	builder, err = NewBuilder(map[string]Factory{
		"openai": FactoryFunc(func(
			context.Context,
			ProviderInput,
		) (inference.ProviderDefinition, error) {
			return inference.ProviderDefinition{}, errors.New("must not run")
		}),
	}, nil)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := builder.Build(t.Context(), document); err == nil ||
		!strings.Contains(err.Error(), `resolver "env"`) {
		t.Fatalf("missing resolver error = %v", err)
	}
}

func TestSecretIsOwnedAndAlwaysRedacted(t *testing.T) {
	source := []byte("secret-value")
	secret, err := NewSecret(source)
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	source[0] = 'X'
	revealed := secret.Bytes()
	revealed[0] = 'X'
	if string(secret.Bytes()) != "secret-value" {
		t.Fatal("Secret retained caller-owned storage")
	}
	if got := fmt.Sprintf("%v", secret); got != "<redacted>" {
		t.Fatalf("formatted secret = %q", got)
	}
	if _, err := json.Marshal(secret); err == nil {
		t.Fatal("Secret was JSON serializable")
	}
}

func TestNewBuilderRejectsNilCatalogEntries(t *testing.T) {
	if _, err := NewBuilder(
		map[string]Factory{"openai": nil},
		nil,
	); err == nil {
		t.Fatal("NewBuilder accepted nil factory")
	}
	if _, err := NewBuilder(
		nil,
		map[string]SecretResolver{"env": nil},
	); err == nil {
		t.Fatal("NewBuilder accepted nil resolver")
	}
}

func TestBuilderDoesNotExposeFactoryOrResolverErrorText(t *testing.T) {
	const sentinel = "secret-that-must-not-escape"
	document := Document{
		Version: VersionV1,
		Providers: []ProviderConfig{{
			ID:     "openai",
			Driver: "openai",
			Profiles: []ProfileConfig{{
				Secrets: map[string]SecretRef{
					"api_key": {Resolver: "env", Key: "OPENAI_API_KEY"},
				},
			}},
		}},
	}
	factory := FactoryFunc(func(
		context.Context,
		ProviderInput,
	) (inference.ProviderDefinition, error) {
		return inference.ProviderDefinition{}, errors.New(sentinel)
	})
	resolver := SecretResolverFunc(func(
		context.Context,
		string,
	) (Secret, error) {
		return NewSecret([]byte(sentinel))
	})
	builder, err := NewBuilder(
		map[string]Factory{"openai": factory},
		map[string]SecretResolver{"env": resolver},
	)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := builder.Build(t.Context(), document); err == nil ||
		strings.Contains(err.Error(), sentinel) {
		t.Fatalf("factory error exposed secret text: %v", err)
	}

	resolverFailure := errors.New(sentinel)
	builder, err = NewBuilder(
		map[string]Factory{"openai": factory},
		map[string]SecretResolver{
			"env": SecretResolverFunc(func(
				context.Context,
				string,
			) (Secret, error) {
				return Secret{}, resolverFailure
			}),
		},
	)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if _, err := builder.Build(t.Context(), document); err == nil ||
		strings.Contains(err.Error(), sentinel) ||
		!errors.Is(err, resolverFailure) {
		t.Fatalf("resolver error handling = %v", err)
	}
}
