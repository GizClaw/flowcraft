package config

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

func TestLocalDeployFactory(t *testing.T) {
	factory := NewLocalDeployFactory()
	if got, want := factory.Spec(), (deploy.ResourceSpec{Kind: ResourceKind, Impl: LocalImpl}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec = %+v, want %+v", got, want)
	}
	value, err := factory.New(context.Background(), deploy.ResourceInput{})
	if err != nil {
		t.Fatal(err)
	}
	server, ok := value.(sdkscheduler.Server)
	if !ok || server == nil {
		t.Fatalf("value = %T, want scheduler.Server", value)
	}
	closer, ok := value.(interface{ Close() error })
	if !ok {
		t.Fatalf("value = %T, want Close() error", value)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalDeployFactoryRejectsSettings(t *testing.T) {
	document, err := deploy.Parse([]byte(`
version: v1
resources:
  scheduler:
    kind: scheduler.Server
    impl: local
    settings: {unexpected: true}
agents: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	factory := NewLocalDeployFactory()
	_, err = factory.New(context.Background(), deploy.ResourceInput{
		Settings: document.Resources["scheduler"].Settings.Node(),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New error = %v, want validation", err)
	}
}
