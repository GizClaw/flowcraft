package keychain

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/secret"
)

// settingsRecorder captures the resolved value of one `${secret:...}`
// reference, so the test sees what the deployment's expansion pass
// handed a consumer.
type settingsRecorder struct{}

type settingsRecorderFactory struct{ records *[]string }

func (f settingsRecorderFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: "deploy.test", Impl: "record",
		Deps: []resource.DepSpec{{Name: "secrets", Type: "secret.Store", Required: false}},
	}
}

func (f settingsRecorderFactory) New(ctx context.Context, in resource.Input) (any, error) {
	var settings struct {
		Token resource.Secret `json:"token"`
	}
	if err := resource.DecodeSettings(ctx, &settings, in.Settings); err != nil {
		return nil, err
	}
	value, err := settings.Token.Resolve(ctx, in.Secrets)
	if err != nil {
		return nil, err
	}
	*f.records = append(*f.records, value)
	return &settingsRecorder{}, nil
}

// The deploy-level shape the guide documents, end to end: the
// secret.Store/keychain resource builds with `id` + `dir`, and a
// consumer's `${secret:keychain.NAME}` resolves to the value the store
// sealed.
func TestBuilderKeychainSecretStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Set(ctx, "token", "sealed-token"); err != nil {
		t.Fatal(err)
	}
	settings, err := json.Marshal(map[string]string{"id": "keychain", "dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	var records []string
	reg := resource.NewRegistry()
	if err := secret.Register(reg); err != nil {
		t.Fatalf("secret.Register: %v", err)
	}
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg.MustRegister(settingsRecorderFactory{records: &records})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"secret.keychain": {
				Kind:     secret.ResourceKind,
				Impl:     ResourceImpl,
				Settings: json.RawMessage(settings),
			},
			"consumer": {
				Kind:     "deploy.test",
				Impl:     "record",
				Settings: json.RawMessage(`{"token": "${secret:keychain.token}"}`),
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(ctx, doc); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(records) != 1 || records[0] != "sealed-token" {
		t.Fatalf("resolved secrets = %v, want the sealed value", records)
	}
}
