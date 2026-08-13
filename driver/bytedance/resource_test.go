package bytedance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestResourceFactoryBuildsProvider(t *testing.T) {
	t.Setenv("BYTEDANCE_TEST_KEY", "sk-test")
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{
			"id": "bytedance",
			"spec": {},
			"profiles": [{
				"id": "default",
				"secrets": {"api_key": "${env:BYTEDANCE_TEST_KEY}"}
			}]
		}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider, ok := value.(inference.ProviderDefinition)
	if !ok {
		t.Fatalf("New returned %T, want inference.ProviderDefinition", value)
	}
	if provider.ID != "bytedance" || len(provider.Models) == 0 {
		t.Fatalf("provider = %+v", provider)
	}
}

func TestResourceFactoryRejectsASRAndRealtime(t *testing.T) {
	for _, kind := range []string{"asr", "realtime"} {
		_, err := Factory().New(context.Background(), resource.Input{
			Settings: json.RawMessage(`{
				"id": "bytedance",
				"spec": {"models": [{"name": "model", "kind": "` + kind + `"}]},
				"profiles": [{
					"id": "default",
					"secrets": {"api_key": "sk-test"}
				}]
			}`),
		})
		if err == nil {
			t.Fatalf("New accepted kind %q before core transcription/realtime", kind)
		}
	}
}

func TestRegisterAddsByTedanceProviderFactory(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Lookup(ResourceKind, "bytedance"); !ok {
		t.Fatalf("factory %s/bytedance missing", ResourceKind)
	}
}
