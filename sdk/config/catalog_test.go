package config

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type widget struct{ name string }

func widgetFactory(name string) Factory[Input, *widget] {
	return func(context.Context, Input) (*widget, error) {
		return &widget{name: name}, nil
	}
}

func TestCatalogRegisterAndBuild(t *testing.T) {
	catalog := NewCatalog[Input, *widget]()
	if err := catalog.Register("a", widgetFactory("a")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := catalog.Register("b", widgetFactory("b")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got, want := catalog.Names(), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	out, err := catalog.Build(context.Background(), "b", Input{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out.name != "b" {
		t.Fatalf("Build returned %q, want b", out.name)
	}
}

func TestCatalogRegisterRejectsBadInput(t *testing.T) {
	catalog := NewCatalog[Input, *widget]()
	if err := catalog.Register("", widgetFactory("x")); !errdefs.IsValidation(err) {
		t.Fatalf("empty name error = %v, want Validation", err)
	}
	if err := catalog.Register("x", nil); !errdefs.IsValidation(err) {
		t.Fatalf("nil factory error = %v, want Validation", err)
	}
	if err := catalog.Register("x", widgetFactory("x")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := catalog.Register("x", widgetFactory("y")); !errdefs.IsValidation(err) {
		t.Fatalf("duplicate error = %v, want Validation", err)
	}
}

func TestCatalogBuildMissingFactory(t *testing.T) {
	catalog := NewCatalog[Input, *widget]()
	if _, err := catalog.Build(context.Background(), "missing", Input{}); !errdefs.IsNotFound(err) {
		t.Fatalf("Build missing error = %v, want NotFound", err)
	}
}

func TestCatalogBuildPreservesFactoryErrorClassification(t *testing.T) {
	catalog := NewCatalog[Input, *widget]()
	if err := catalog.Register("bad", func(context.Context, Input) (*widget, error) {
		return nil, errdefs.NotAvailablef("backend offline")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Build(context.Background(), "bad", Input{}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Build error = %v, want preserved NotAvailable", err)
	}
	if err := catalog.Register("plain", func(context.Context, Input) (*widget, error) {
		return nil, errors.New("boom")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Build(context.Background(), "plain", Input{}); err == nil {
		t.Fatal("Build accepted a failing factory")
	}
}

func TestCatalogBuildRejectsNilOutput(t *testing.T) {
	catalog := NewCatalog[Input, *widget]()
	if err := catalog.Register("nil", func(context.Context, Input) (*widget, error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Build(context.Background(), "nil", Input{}); !errdefs.IsInternal(err) {
		t.Fatalf("Build nil output error = %v, want Internal", err)
	}
}
