package dashscope

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
)

// providerSettings renders one provider resource whose spec declares a
// text2image model and a video model, wired to a literal api_key secret.
func providerSettings(t *testing.T) json.RawMessage {
	t.Helper()
	settings := map[string]any{
		"id": "dashscope",
		"spec": map[string]any{
			"base_url":             "https://dashscope.aliyuncs.com",
			"workspace_id":         "ws-1",
			"poll_interval_millis": 1500,
			"models": []map[string]any{
				{
					"name": "wanx-v1",
					"kind": "image",
					"capabilities": map[string]any{
						"inputs":  []string{"text", "image"},
						"outputs": []string{"image"},
					},
					"image": map[string]any{
						"reference_image": true,
						"prompt_limit":    800,
						"sizes":           []string{"1024*1024"},
					},
				},
				{
					"name":       "wan2.2-t2v-plus",
					"kind":       "video",
					"wire_model": "wan2.2-t2v-plus-2026-03-01",
					"capabilities": map[string]any{
						"inputs":  []string{"text"},
						"outputs": []string{"video"},
					},
					"video": map[string]any{
						"resolutions":  []string{"480P", "1080P"},
						"ratios":       []string{"16:9"},
						"durations":    []int{5},
						"prompt_limit": 800,
					},
				},
			},
		},
		"profiles": []map[string]any{{
			"id":         "beijing",
			"operations": []string{"generate"},
			"secrets": map[string]any{
				"api_key": "sk-test",
			},
			"spec": map[string]any{"workspace_id": "ws-beijing"},
		}},
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	return raw
}

// declaredModel returns the model the provider declares under name, so a test
// can address a model without depending on the catalog's sorted order.
func declaredModel(
	t *testing.T,
	provider inference.ProviderDefinition,
	name string,
) inference.ModelImplementation {
	t.Helper()
	for _, candidate := range provider.Models {
		if candidate.Descriptor.ID.Name == name {
			return candidate
		}
	}
	t.Fatalf("the provider declares no model %q", name)
	return inference.ModelImplementation{}
}

func TestFactoryBuildsProvider(t *testing.T) {
	factory := Factory()
	if spec := factory.Spec(); spec.Kind != ResourceKind || spec.Impl != "dashscope" {
		t.Fatalf("factory spec = %+v", spec)
	}
	value, err := factory.New(context.Background(), resource.Input{
		Settings: providerSettings(t),
	})
	if err != nil {
		t.Fatalf("factory.New: %v", err)
	}
	provider, ok := value.(inference.ProviderDefinition)
	if !ok {
		t.Fatalf("factory returned %T", value)
	}
	if provider.ID != "dashscope" {
		t.Errorf("provider id = %q", provider.ID)
	}
	if len(provider.Profiles) != 1 || provider.Profiles[0].ID != "beijing" {
		t.Fatalf("profiles = %+v", provider.Profiles)
	}
	if len(provider.Models) != 2 {
		t.Fatalf("models = %+v", provider.Models)
	}
	// Declarations sort by name, so the settings order does not decide the
	// catalog order: "wan2.2-t2v-plus" leads "wanx-v1".
	if provider.Models[0].Descriptor.ID.Name != "wan2.2-t2v-plus" ||
		provider.Models[1].Descriptor.ID.Name != "wanx-v1" {
		t.Fatalf("model order = %q, %q",
			provider.Models[0].Descriptor.ID.Name,
			provider.Models[1].Descriptor.ID.Name)
	}
	image := declaredModel(t, provider, "wanx-v1")
	video := declaredModel(t, provider, "wan2.2-t2v-plus")
	if video.Descriptor.ID.Provider != "dashscope" {
		t.Errorf("model provider = %q", video.Descriptor.ID.Provider)
	}
	for _, name := range []string{extensionImage, extensionVideo} {
		if _, ok := provider.ExtensionDecoders[name]; !ok {
			t.Errorf("extension decoders are missing %q", name)
		}
	}
	if image.Openers.Generate == nil || video.Openers.Generate == nil {
		t.Fatal("a model has no generate opener")
	}
}

func TestFactoryModelOpenerServesWire(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: providerSettings(t),
	})
	if err != nil {
		t.Fatalf("factory.New: %v", err)
	}
	provider := value.(inference.ProviderDefinition)
	video := declaredModel(t, provider, "wan2.2-t2v-plus")

	// The video model aliases its wire token, so the compiled payload has to
	// carry the alias rather than the deployment name.
	operations, err := video.Openers.Generate(
		context.Background(),
		model.ModelRef{ID: video.Descriptor.ID, Profile: "beijing"},
	)
	if err != nil {
		t.Fatalf("open generate: %v", err)
	}
	if operations.Unary == nil {
		t.Fatal("the video model opened no unary driver")
	}
	if operations.Stream != nil {
		t.Fatal("the video model opened a stream driver")
	}
	// Explain compiles without performing provider I/O, so it proves the
	// opener's compiler serves the declared surface.
	explanation, err := operations.Unary.Explain(
		context.Background(),
		model.ModelRef{ID: video.Descriptor.ID, Profile: "beijing"},
		inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "a paper boat"},
					}},
					Intent: inference.Intent{Video: &inference.VideoIntent{
						Resolution: "1080p",
					}},
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if explanation.Operation != model.OperationGenerate {
		t.Fatalf("operation = %q", explanation.Operation)
	}
	for _, decision := range explanation.Decisions {
		if decision.Disposition == inference.Rejected {
			t.Fatalf("decision = %+v", decision)
		}
	}
}

func TestFactoryRejectsBadSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		reason   string
	}{
		{
			name:     "missing provider id",
			settings: `{"spec":{"models":[]}}`,
			reason:   "settings.id is required",
		},
		{
			name:     "unknown secret",
			settings: `{"id":"dashscope","profiles":[{"id":"p","secrets":{"token":"x"}}]}`,
			reason:   "unknown secret",
		},
		{
			name:     "profile without an id",
			settings: `{"id":"dashscope","profiles":[{"secrets":{"api_key":"x"}}]}`,
			reason:   "missing its id",
		},
		{
			name:     "unsupported kind",
			settings: `{"id":"dashscope","spec":{"models":[{"name":"qwen3.8-max","kind":"generate","capabilities":{"outputs":["text"]}}]}}`,
			reason:   "unsupported kind",
		},
		{
			name:     "image parameters on a video model",
			settings: `{"id":"dashscope","spec":{"models":[{"name":"wan2.2-t2v-plus","kind":"video","capabilities":{"outputs":["video"]},"image":{"prompt_limit":10}}]}}`,
			reason:   "image parameters on kind",
		},
		{
			name:     "image family without image output",
			settings: `{"id":"dashscope","spec":{"models":[{"name":"wanx-v1","kind":"image","capabilities":{"outputs":["text"]}}]}}`,
			reason:   "must declare image output",
		},
		{
			name:     "reference image and edit together",
			settings: `{"id":"dashscope","spec":{"models":[{"name":"wanx-v1","kind":"image","capabilities":{"outputs":["image"]},"image":{"reference_image":true,"edit":true}}]}}`,
			reason:   "no single home",
		},
		{
			name:     "negative size bound",
			settings: `{"id":"dashscope","spec":{"models":[{"name":"wanx-v1","kind":"image","capabilities":{"outputs":["image"]},"image":{"min_edge":-1}}]}}`,
			reason:   "must not be negative",
		},
		{
			name:     "unknown video input shape",
			settings: `{"id":"dashscope","spec":{"models":[{"name":"wan2.2-t2v-plus","kind":"video","capabilities":{"outputs":["video"]},"video":{"input":"fl2v"}}]}}`,
			reason:   "unknown input",
		},
		{
			name:     "lowercase resolution tier",
			settings: `{"id":"dashscope","spec":{"models":[{"name":"wan2.2-t2v-plus","kind":"video","capabilities":{"outputs":["video"]},"video":{"resolutions":["720p"]}}]}}`,
			reason:   "tier token like 720P",
		},
		{
			name:     "duplicate model",
			settings: `{"id":"dashscope","spec":{"models":[{"name":"wanx-v1","kind":"image","capabilities":{"outputs":["image"]}},{"name":"wanx-v1","kind":"image","capabilities":{"outputs":["image"]}}]}}`,
			reason:   "duplicate model",
		},
		{
			name:     "hosted web search",
			settings: `{"id":"dashscope","spec":{"models":[{"name":"wanx-v1","kind":"image","capabilities":{"outputs":["image"],"hosted_web_search":true}}]}}`,
			reason:   "hosted_web_search",
		},
		{
			name:     "base url that is not a url",
			settings: `{"id":"dashscope","spec":{"base_url":"dashscope.aliyuncs.com"}}`,
			reason:   "must be an http(s) URL",
		},
		{
			name:     "workspace id with a slash",
			settings: `{"id":"dashscope","spec":{"workspace_id":"ws/1"}}`,
			reason:   "invalid workspace_id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Factory().New(context.Background(), resource.Input{
				Settings: json.RawMessage(tc.settings),
			})
			if err == nil {
				t.Fatal("factory accepted invalid settings")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error = %v, want substring %q", err, tc.reason)
			}
		})
	}
}

func TestFactoryUndeclaredProfileRejects(t *testing.T) {
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: providerSettings(t),
	})
	if err != nil {
		t.Fatalf("factory.New: %v", err)
	}
	provider := value.(inference.ProviderDefinition)
	_, err = provider.Models[0].Openers.Generate(
		context.Background(),
		model.ModelRef{ID: provider.Models[0].Descriptor.ID, Profile: "singapore"},
	)
	if err == nil {
		t.Fatal("opening an undeclared profile succeeded")
	}
	if !strings.Contains(err.Error(), "undeclared profile") {
		t.Errorf("error = %v", err)
	}
}

func TestRegisterAddsFactory(t *testing.T) {
	registry := resource.NewRegistry()
	if err := Register(registry); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := registry.Lookup(ResourceKind, "dashscope"); !ok {
		t.Fatal("the registry has no dashscope factory")
	}
}

func TestDecodeProfileSpecAppliesProfileOverrides(t *testing.T) {
	// A profile may pin its own region host and workspace: keys are
	// region-bound, so two regions in one deployment need two profiles.
	material, err := newProfileMaterial(context.Background(), ProfileSettings{
		ID: "singapore",
		Spec: json.RawMessage(
			`{"base_url":"https://dashscope-intl.aliyuncs.com/v1","workspace_id":"ws-sg"}`,
		),
		Secrets: map[string]resource.Secret{
			SecretAPIKey: resource.LiteralSecret("sk-sg"),
		},
	}, nil)
	if err != nil {
		t.Fatalf("newProfileMaterial: %v", err)
	}
	clients, err := material.newClients(context.Background(), Spec{
		BaseURL:     "https://dashscope.aliyuncs.com",
		WorkspaceID: "ws-cn",
	})
	if err != nil {
		t.Fatalf("newClients: %v", err)
	}
	if clients.media.base != "https://dashscope-intl.aliyuncs.com/v1" {
		t.Errorf("base = %q, want the profile host", clients.media.base)
	}
	if clients.media.workspace != "ws-sg" {
		t.Errorf("workspace = %q, want the profile workspace", clients.media.workspace)
	}
	if clients.media.key != "sk-sg" {
		t.Errorf("key = %q", clients.media.key)
	}
}

func TestNewProfileMaterialRejectsMissingKey(t *testing.T) {
	material, err := newProfileMaterial(context.Background(), ProfileSettings{
		ID: "beijing",
	}, nil)
	if err != nil {
		t.Fatalf("newProfileMaterial: %v", err)
	}
	if _, err := material.newClients(context.Background(), Spec{}); err == nil {
		t.Fatal("a profile without an api_key opened clients")
	}
}

func TestSanitizeWireModelRoundTripsDeclaredName(t *testing.T) {
	declared := ModelSpec{Name: "wanx-v1", Kind: "image"}
	if got := wireModel("wanx-v1", declared); got != "wanx-v1" {
		t.Errorf("wireModel = %q", got)
	}
	declared.WireModel = "wanx-v1-2026-01-01"
	if got := wireModel("wanx-v1", declared); got != "wanx-v1-2026-01-01" {
		t.Errorf("wireModel = %q", got)
	}
}

func TestFactoryWireModelAliasLowers(t *testing.T) {
	spec, err := decodeSpec(context.Background(), []byte(
		`{"models":[{"name":"wan2.2-t2v-plus","kind":"video",`+
			`"wire_model":"wan2.2-t2v-plus-2026-03-01",`+
			`"capabilities":{"outputs":["video"]},`+
			`"video":{"resolutions":["1080P"],"durations":[5]}}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	declared := spec.Models[0]
	wire, report, err := compileVideoWire(t, declared, userRequest(
		[]message.Part{message.TextPart{Text: "a paper boat"}},
		inference.Intent{Video: &inference.VideoIntent{
			Resolution:     "1080p",
			DurationMillis: ptrTo(int64(5000)),
		}},
	))
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if wire.model != "wan2.2-t2v-plus-2026-03-01" {
		t.Errorf("wire model = %q, want the declared alias", wire.model)
	}
}
