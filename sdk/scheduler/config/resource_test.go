package config_test

import (
	"context"
	"reflect"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	schedulerconfig "github.com/GizClaw/flowcraft/sdk/scheduler/config"
)

type fakeServer struct{}

func (fakeServer) PutRule(context.Context, sdkscheduler.Rule) error { return nil }
func (fakeServer) DeleteRule(context.Context, string, string) error { return nil }
func (fakeServer) ListRules(context.Context, string) ([]sdkscheduler.Rule, error) {
	return nil, nil
}
func (fakeServer) ScheduleOnce(context.Context, sdkscheduler.Once) error { return nil }
func (fakeServer) CancelOnce(context.Context, string, string) error      { return nil }
func (fakeServer) Claim(context.Context, sdkscheduler.ClaimRequest) (*sdkscheduler.Delivery, error) {
	return nil, nil
}
func (fakeServer) Renew(context.Context, sdkscheduler.RenewRequest) error { return nil }
func (fakeServer) Complete(context.Context, sdkscheduler.CompleteRequest) error {
	return nil
}

var _ sdkscheduler.Server = fakeServer{}

func TestDeployFactorySpec(t *testing.T) {
	builder := schedulerconfig.NewBuilder()
	if err := builder.RegisterFactory("fake", func(
		context.Context,
		sdkconfig.Input,
	) (sdkscheduler.Server, error) {
		return fakeServer{}, nil
	}); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	got := schedulerconfig.NewDeployFactory("fake", builder).Spec()
	want := sdkconfig.Spec{Kind: schedulerconfig.ResourceKind, Impl: "fake"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
}

func TestDeployFactoryNewBuildsServer(t *testing.T) {
	builder := schedulerconfig.NewBuilder()
	if err := builder.RegisterFactory("fake", func(
		context.Context,
		sdkconfig.Input,
	) (sdkscheduler.Server, error) {
		return fakeServer{}, nil
	}); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	value, err := schedulerconfig.NewDeployFactory("fake", builder).New(
		context.Background(),
		sdkconfig.Input{},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(sdkscheduler.Server); !ok {
		t.Fatalf("New returned %T, want scheduler.Server", value)
	}
}

func TestDeployFactoryRejectsUnregisteredImplAndNilBuilder(t *testing.T) {
	builder := schedulerconfig.NewBuilder()
	if _, err := schedulerconfig.NewDeployFactory("missing", builder).New(
		context.Background(),
		sdkconfig.Input{},
	); err == nil {
		t.Fatal("New accepted an unregistered implementation")
	}
	if _, err := schedulerconfig.NewDeployFactory("fake", nil).New(
		context.Background(),
		sdkconfig.Input{},
	); err == nil {
		t.Fatal("New accepted a nil builder")
	}
}
