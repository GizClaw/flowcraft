package config

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

func TestParseAcceptsVersionedSecretReferences(t *testing.T) {
	for name, data := range map[string]string{
		"json": `{
			"version":"v1",
			"providers":[{
				"id":"openai",
				"driver":"openai",
				"profiles":[{
					"id":"default",
					"operations":["generate","embed"],
					"secrets":{
						"api_key":{"resolver":"env","key":"OPENAI_API_KEY"}
					},
					"spec":{"base_url":"https://example.com/v1"}
				}],
				"spec":{"models":[{"name":"custom-model","preset":"gpt-5"}]}
			}]
		}`,
		"yaml": `version: v1
providers:
  - id: openai
    driver: openai
    profiles:
      - id: default
        operations: [generate, embed]
        secrets:
          api_key: {resolver: env, key: OPENAI_API_KEY}
        spec:
          base_url: https://example.com/v1
    spec:
      models:
        - {name: custom-model, preset: gpt-5}
`,
	} {
		t.Run(name, func(t *testing.T) {
			document, err := Parse([]byte(data))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			provider := document.Providers[0]
			if provider.ID != "openai" || provider.Driver != "openai" {
				t.Fatalf("provider = %+v", provider)
			}
			profile := provider.Profiles[0]
			if len(profile.Operations) != 2 ||
				profile.Operations[0] != inference.OperationGenerate ||
				profile.Secrets["api_key"].Key != "OPENAI_API_KEY" {
				t.Fatalf("profile = %+v", profile)
			}
		})
	}
}

func TestParseRejectsInlineSecretsAndUnknownEnvelopeFields(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{
			name: "inline secret value",
			data: `{
				"version":"v1",
				"providers":[{
					"id":"openai",
					"driver":"openai",
					"profiles":[{
						"secrets":{"api_key":{
							"resolver":"env",
							"key":"OPENAI_API_KEY",
							"value":"secret"
						}}
					}]
				}]
			}`,
		},
		{
			name: "unknown provider field",
			data: `{
				"version":"v1",
				"providers":[{
					"id":"openai",
					"driver":"openai",
					"api_key":"secret"
				}]
			}`,
		},
		{
			name: "credential in provider spec",
			data: `{
				"version":"v1",
				"providers":[{
					"id":"openai",
					"driver":"openai",
					"spec":{"api_key":"secret"}
				}]
			}`,
		},
		{
			name: "nested credential in profile spec",
			data: `{
				"version":"v1",
				"providers":[{
					"id":"openai",
					"driver":"openai",
					"profiles":[{
						"spec":{"auth":{"access_token":"secret"}}
					}]
				}]
			}`,
		},
		{
			name: "multiple yaml documents",
			data: "version: v1\nproviders:\n  - {id: a, driver: a}\n---\nversion: v1\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.data)); err == nil {
				t.Fatal("Parse accepted an invalid document")
			}
		})
	}
}

func TestDocumentValidateRejectsInvalidConfiguration(t *testing.T) {
	valid := Document{
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
	tests := []struct {
		name   string
		mutate func(*Document)
	}{
		{"unknown version", func(document *Document) {
			document.Version = "v2"
		}},
		{"duplicate provider", func(document *Document) {
			document.Providers = append(
				document.Providers,
				document.Providers[0],
			)
		}},
		{"duplicate profile", func(document *Document) {
			document.Providers[0].Profiles = append(
				document.Providers[0].Profiles,
				document.Providers[0].Profiles[0],
			)
		}},
		{"invalid operation", func(document *Document) {
			document.Providers[0].Profiles[0].Operations =
				[]inference.Operation{"unknown"}
		}},
		{"missing resolver", func(document *Document) {
			document.Providers[0].Profiles[0].Secrets["api_key"] =
				SecretRef{Key: "OPENAI_API_KEY"}
		}},
		{"invalid provider spec", func(document *Document) {
			document.Providers[0].Spec = json.RawMessage(`[]`)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			document := valid.Clone()
			tt.mutate(&document)
			if err := document.Validate(); err == nil {
				t.Fatal("Validate accepted invalid configuration")
			}
		})
	}
}

func TestDocumentCloneOwnsNestedConfiguration(t *testing.T) {
	original := Document{
		Version: VersionV1,
		Providers: []ProviderConfig{{
			ID:     "openai",
			Driver: "openai",
			Spec:   json.RawMessage(`{"models":["a"]}`),
			Profiles: []ProfileConfig{{
				Operations: []inference.Operation{inference.OperationGenerate},
				Secrets: map[string]SecretRef{
					"api_key": {Resolver: "env", Key: "OPENAI_API_KEY"},
				},
				Spec: json.RawMessage(`{"base_url":"https://example.com"}`),
			}},
		}},
	}
	cloned := original.Clone()
	cloned.Providers[0].Spec[0] = '['
	cloned.Providers[0].Profiles[0].Operations[0] = inference.OperationEmbed
	cloned.Providers[0].Profiles[0].Secrets["api_key"] =
		SecretRef{Resolver: "env", Key: "CHANGED"}
	cloned.Providers[0].Profiles[0].Spec[0] = '['

	if string(original.Providers[0].Spec) != `{"models":["a"]}` ||
		original.Providers[0].Profiles[0].Operations[0] !=
			inference.OperationGenerate ||
		original.Providers[0].Profiles[0].Secrets["api_key"].Key !=
			"OPENAI_API_KEY" ||
		string(original.Providers[0].Profiles[0].Spec) !=
			`{"base_url":"https://example.com"}` {
		t.Fatalf("original was mutated: %+v", original)
	}
}

func TestDecodeSpecIsProviderOwnedAndStrict(t *testing.T) {
	type providerSpec struct {
		BaseURL string `json:"base_url"`
	}
	decoded, err := DecodeSpec[providerSpec](
		json.RawMessage(`{"base_url":"https://example.com"}`),
	)
	if err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
	if decoded.BaseURL != "https://example.com" {
		t.Fatalf("decoded = %+v", decoded)
	}
	if _, err := DecodeSpec[providerSpec](
		json.RawMessage(`{"api_key":"forbidden"}`),
	); err == nil {
		t.Fatal("DecodeSpec accepted unknown provider field")
	}
}

func TestParseRejectsMultipleJSONValues(t *testing.T) {
	if _, err := Parse([]byte(`{"version":"v1","providers":[{"id":"a","driver":"a"}]} {"version":"v1"}`)); err == nil {
		t.Fatal("Parse accepted multiple JSON values")
	}
}
