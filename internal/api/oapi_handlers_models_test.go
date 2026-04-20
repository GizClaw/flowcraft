package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/internal/api/oas"
	"github.com/GizClaw/flowcraft/internal/platform"
	"github.com/GizClaw/flowcraft/internal/store"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/go-faster/jx"
)

// newModelHandler wires a real SQLiteStore into a minimal Platform so
// AddModel/ListModels/SetDefaultModel/DeleteModel exercise the same
// store path production uses (the auth_test stub doesn't implement
// the new ModelConfigStore / DefaultModelStore methods).
func newModelHandler(t *testing.T) (*oapiHandler, *store.SQLiteStore) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.NewSQLiteStore(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	srv := &Server{deps: ServerDeps{Platform: &platform.Platform{Store: s}}}
	return newOAPIHandler(srv), s
}

func optStr(v string) oas.OptString { return oas.NewOptString(v) }

// jsonObj builds an OptJSONObject from a free-form map by JSON-encoding
// each value to jx.Raw, mirroring how ogen would deserialize a real
// HTTP body. Keeps test cases readable instead of forcing callers to
// hand-build jx.Raw values.
func jsonObj(m map[string]any) oas.OptJSONObject {
	out := oas.JSONObject{}
	for k, v := range m {
		raw, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		out[k] = jx.Raw(raw)
	}
	return oas.OptJSONObject{Set: true, Value: out}
}

// TestAddModel_Caps_PersistedTyped is the regression test for B1: the
// AddModel handler used to flatten `extra.caps` into the per-model row
// as untyped JSON, where the resolver never read it. After the
// migration-005 split the handler must split caps off into the typed
// ModelConfig.Caps field — verifying that here means the resolver will
// actually pick them up via store.GetModelConfig.
func TestAddModel_Caps_PersistedTyped(t *testing.T) {
	h, s := newModelHandler(t)
	ctx := context.Background()

	extra := jsonObj(map[string]any{
		"caps":       map[string]any{"disabled": map[string]any{"temperature": true}},
		"max_tokens": float64(2048),
	})
	if _, err := h.AddModel(ctx, &oas.AddModelRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		APIKey:   optStr("sk-test"),
		Extra:    extra,
	}); err != nil {
		t.Fatalf("AddModel: %v", err)
	}

	mc, err := s.GetModelConfig(ctx, "openai", "gpt-4o")
	if err != nil {
		t.Fatalf("GetModelConfig: %v", err)
	}
	if !mc.Caps.Disabled[llm.CapTemperature] {
		t.Errorf("caps lost; mc.Caps=%+v", mc.Caps)
	}
	if mc.Extra["max_tokens"] != float64(2048) {
		t.Errorf("extra lost; mc.Extra=%+v", mc.Extra)
	}
	// `caps` must NOT remain in Extra (it was lifted out).
	if _, leaked := mc.Extra["caps"]; leaked {
		t.Errorf("caps key leaked into Extra: %+v", mc.Extra)
	}

	// Provider-level credentials landed on the right table.
	pc, err := s.GetProviderConfig(ctx, "openai")
	if err != nil {
		t.Fatalf("GetProviderConfig: %v", err)
	}
	if pc.Config["api_key"] != "sk-test" {
		t.Errorf("api_key lost; pc=%+v", pc.Config)
	}
}

// TestAddModel_CredsMergeNotReplace guards against the bug where a
// second AddModel call for the same provider would wipe credentials
// set by the first. The UI doesn't force users to retype api_key when
// adding a second model under the same provider, so a replace-style
// SetProviderConfig would silently break every other model that
// provider is serving.
func TestAddModel_CredsMergeNotReplace(t *testing.T) {
	h, s := newModelHandler(t)
	ctx := context.Background()

	if _, err := h.AddModel(ctx, &oas.AddModelRequest{
		Provider: "openai", Model: "gpt-4o",
		APIKey:  optStr("sk-test"),
		BaseURL: optStr("https://api.openai.com"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddModel(ctx, &oas.AddModelRequest{
		Provider: "openai", Model: "gpt-4-turbo",
		// no APIKey/BaseURL — UI flow for adding a second model to an
		// already-configured provider.
	}); err != nil {
		t.Fatal(err)
	}

	pc, err := s.GetProviderConfig(ctx, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if pc.Config["api_key"] != "sk-test" {
		t.Errorf("api_key wiped by second AddModel: %+v", pc.Config)
	}
	if pc.Config["base_url"] != "https://api.openai.com" {
		t.Errorf("base_url wiped: %+v", pc.Config)
	}
}

// TestSetDefaultModel_RoundTrip covers SetDefaultModel + ListModels
// surfacing the is_default flag. SetDefaultModel must validate that
// the target model has an existing model_configs row, otherwise the
// resolver would dangle.
func TestSetDefaultModel_RoundTrip(t *testing.T) {
	h, s := newModelHandler(t)
	ctx := context.Background()

	if err := h.SetDefaultModel(ctx, &oas.SetDefaultModelRequest{
		Provider: "openai", Model: "ghost",
	}); !errdefs.IsValidation(err) {
		t.Errorf("setting default to unconfigured model: want Validation, got %v", err)
	}

	if _, err := h.AddModel(ctx, &oas.AddModelRequest{Provider: "openai", Model: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	if err := h.SetDefaultModel(ctx, &oas.SetDefaultModelRequest{
		Provider: "openai", Model: "gpt-4o",
	}); err != nil {
		t.Fatal(err)
	}

	ref, err := s.GetDefaultModel(ctx)
	if err != nil || ref.Provider != "openai" || ref.Model != "gpt-4o" {
		t.Errorf("default not persisted: %+v err=%v", ref, err)
	}

	list, err := h.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 {
		t.Fatalf("expected 1 model, got %d", len(list.Data))
	}
	if got, _ := list.Data[0].IsDefault.Get(); !got {
		t.Errorf("is_default=false on the only model")
	}
}

// TestDeleteModel_ClearsDefault asserts the cleanup behavior: when the
// row backing the default-model pointer is removed, the pointer must
// be cleared so Resolve("") doesn't dangle. This is the contract
// metatool/node.go and the resolver both rely on.
func TestDeleteModel_ClearsDefault(t *testing.T) {
	h, s := newModelHandler(t)
	ctx := context.Background()

	if _, err := h.AddModel(ctx, &oas.AddModelRequest{Provider: "openai", Model: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	if err := h.SetDefaultModel(ctx, &oas.SetDefaultModelRequest{Provider: "openai", Model: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	if err := h.DeleteModel(ctx, oas.DeleteModelParams{ModelID: "openai/gpt-4o"}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetModelConfig(ctx, "openai", "gpt-4o"); !errdefs.IsNotFound(err) {
		t.Errorf("model not deleted: %v", err)
	}
	if _, err := s.GetDefaultModel(ctx); !errdefs.IsNotFound(err) {
		t.Errorf("default not cleared: %v", err)
	}
}

// TestCapsFromMap covers both UI input shapes the SDK historically
// accepted: structured {"disabled":{...}} and the older flat
// {"no_temperature":true}. Both must produce the same typed caps so
// users can paste either form into the API.
func TestCapsFromMap(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want llm.Capability
	}{
		{
			name: "structured",
			in:   map[string]any{"disabled": map[string]any{"temperature": true}},
			want: llm.CapTemperature,
		},
		{
			name: "flat_no_temperature",
			in:   map[string]any{"no_temperature": true},
			want: llm.CapTemperature,
		},
		{
			name: "flat_no_json_mode",
			in:   map[string]any{"no_json_mode": true},
			want: llm.CapJSONMode,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capsFromMap(tc.in)
			if !got.Disabled[tc.want] {
				t.Errorf("want %s disabled, got %+v", tc.want, got)
			}
		})
	}

	// Empty / unknown payload → zero caps (no-op middleware).
	if got := capsFromMap(map[string]any{}); !got.IsZero() {
		t.Errorf("empty payload should produce zero caps, got %+v", got)
	}
	if got := capsFromMap(map[string]any{"unknown_flag": true}); !got.IsZero() {
		t.Errorf("unknown flag should not produce caps, got %+v", got)
	}
}
