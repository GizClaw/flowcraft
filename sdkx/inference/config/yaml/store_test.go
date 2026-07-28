package yaml

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdkx/inference/config"
)

func TestStoreRoundTripAndOptimisticRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inference.yaml")
	store, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	document := config.Document{
		Version: config.VersionV1,
		Providers: []config.ProviderConfig{{
			ID:     "openai",
			Driver: "openai",
			Spec:   []byte(`{"models":[{"name":"custom"}]}`),
			Profiles: []config.ProfileConfig{{
				ID:         "default",
				Operations: []inference.Operation{inference.OperationGenerate},
				Secrets: map[string]config.SecretRef{
					"api_key": {
						Resolver: "env",
						Key:      "OPENAI_API_KEY",
					},
				},
				Spec: []byte(`{"base_url":"https://example.com/v1"}`),
			}},
		}},
	}
	saved, err := store.Save(t.Context(), "", document)
	if err != nil {
		t.Fatalf("Save create: %v", err)
	}
	if saved.Revision == "" {
		t.Fatal("Save returned an empty revision")
	}
	loaded, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Revision != saved.Revision ||
		string(loaded.Document.Providers[0].Spec) !=
			`{"models":[{"name":"custom"}]}` ||
		loaded.Document.Providers[0].Profiles[0].
			Secrets["api_key"].Key != "OPENAI_API_KEY" {
		t.Fatalf("loaded = %+v", loaded)
	}

	loaded.Document.Providers[0].Driver = "changed"
	again, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load again: %v", err)
	}
	if again.Document.Providers[0].Driver != "openai" {
		t.Fatal("Store returned shared document storage")
	}
	if _, err := store.Save(
		t.Context(),
		"stale-revision",
		document,
	); !errors.Is(err, config.ErrConflict) {
		t.Fatalf("stale Save error = %v", err)
	}
}

func TestStoreRejectsInlineSecretsAndUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inference.yaml")
	if err := os.WriteFile(path, []byte(`
version: v1
providers:
  - id: openai
    driver: openai
    profiles:
      - secrets:
          api_key:
            resolver: env
            key: OPENAI_API_KEY
            value: forbidden
`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := store.Load(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "value") {
		t.Fatalf("Load inline secret error = %v", err)
	}
}

func TestStoreLoadMissing(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := store.Load(t.Context()); !errors.Is(
		err,
		config.ErrNotFound,
	) {
		t.Fatalf("Load missing error = %v", err)
	}
}

func TestStoreCASIsSharedAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inference.yaml")
	first, err := New(path)
	if err != nil {
		t.Fatalf("New first: %v", err)
	}
	second, err := New(path)
	if err != nil {
		t.Fatalf("New second: %v", err)
	}
	document := config.Document{
		Version: config.VersionV1,
		Providers: []config.ProviderConfig{{
			ID:     "openai",
			Driver: "openai",
		}},
	}
	created, err := first.Save(t.Context(), "", document)
	if err != nil {
		t.Fatalf("Save create: %v", err)
	}

	start := make(chan struct{})
	errorsByWriter := make(chan error, 2)
	var writers sync.WaitGroup
	for index, store := range []*Store{first, second} {
		writers.Add(1)
		go func(index int, store *Store) {
			defer writers.Done()
			<-start
			replacement := document.Clone()
			replacement.Providers[0].Driver = []string{
				"azure",
				"bytedance",
			}[index]
			_, err := store.Save(
				t.Context(),
				created.Revision,
				replacement,
			)
			errorsByWriter <- err
		}(index, store)
	}
	close(start)
	writers.Wait()
	close(errorsByWriter)
	var successes, conflicts int
	for err := range errorsByWriter {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, config.ErrConflict):
			conflicts++
		default:
			t.Fatalf("Save error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf(
			"successes=%d conflicts=%d",
			successes,
			conflicts,
		)
	}
}

func TestStorePreservesSpecNumberPrecision(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "inference.yaml"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	document := config.Document{
		Version: config.VersionV1,
		Providers: []config.ProviderConfig{{
			ID:     "custom",
			Driver: "custom",
			Spec: []byte(
				`{"integer":9007199254740993,"decimal":1.234567890123456789}`,
			),
		}},
	}
	if _, err := store.Save(t.Context(), "", document); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got struct {
		Integer json.Number `json:"integer"`
		Decimal json.Number `json:"decimal"`
	}
	decoder := json.NewDecoder(bytes.NewReader(
		loaded.Document.Providers[0].Spec,
	))
	decoder.UseNumber()
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if got.Integer.String() != "9007199254740993" ||
		got.Decimal.String() != "1.234567890123456789" {
		t.Fatalf("spec numbers = %+v", got)
	}
}
