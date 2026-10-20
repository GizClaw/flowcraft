package resource

import (
	"reflect"
	"testing"
)

// wantProvider is a test contract: Dep[T] narrows to its type parameter.
type wantProvider struct{ ID string }

// otherContract never satisfies wantProvider.
type otherContract struct{}

func TestDepGetAllOrderAndKeys(t *testing.T) {
	in := NewTestInput(map[string]any{
		"provider.openai": wantProvider{ID: "openai"},
		"provider.qwen":   wantProvider{ID: "qwen"},
		"provider":        wantProvider{ID: "single"},
		"other":           otherContract{},
	})
	dep := RequireMany[wantProvider]("provider")

	got, err := dep.GetAll(in)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("GetAll = %v, want 3 values", got)
	}
	if got[0].ID != "single" || got[1].ID != "openai" || got[2].ID != "qwen" {
		t.Fatalf("GetAll order = %v, want [single openai qwen]", got)
	}

	named, err := dep.GetAllNamed(in)
	if err != nil {
		t.Fatalf("GetAllNamed: %v", err)
	}
	if len(named) != 3 || named[0].Name != "provider" ||
		named[1].Name != "provider.openai" || named[2].Name != "provider.qwen" {
		t.Fatalf("GetAllNamed = %+v, want the dep keys in sorted order", named)
	}
}

func TestDepGetAllEmpty(t *testing.T) {
	dep := OptionalMany[wantProvider]("provider")
	got, err := dep.GetAll(NewTestInput(map[string]any{"other": otherContract{}}))
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetAll = %v, want empty", got)
	}
	if _, err := RequireMany[wantProvider]("provider").GetAll(NewTestInput(nil)); err == nil {
		t.Fatal("required Many dep accepted an empty dep set")
	}
}

func TestDepGetReportsMissingMistypedAndTypedNil(t *testing.T) {
	dep := Require[wantProvider]("provider")

	if _, err := dep.Get(NewTestInput(nil)); err == nil {
		t.Fatal("Get on a missing dep returned no error")
	}
	if _, ok, err := Optional[wantProvider]("provider").TryGet(NewTestInput(nil)); err != nil || ok {
		t.Fatalf("TryGet on a missing optional dep = (%v, %v), want (false, nil)", ok, err)
	}
	if _, err := dep.Get(NewTestInput(map[string]any{"provider": otherContract{}})); err == nil {
		t.Fatal("Get accepted a value of the wrong type")
	}

	var typedNil *wantProvider
	if _, err := Require[*wantProvider]("provider").Get(NewTestInput(map[string]any{"provider": typedNil})); err == nil {
		t.Fatal("Get accepted a typed nil")
	}
}

func TestDepSpecCarriesTheGoType(t *testing.T) {
	spec := RequireMany[wantProvider]("provider").Spec()
	if spec.Name != "provider" || !spec.Required || !spec.Many {
		t.Fatalf("Spec = %+v, want a required Many provider dep", spec)
	}
	if spec.Type != reflect.TypeFor[wantProvider]() {
		t.Fatalf("Spec.Type = %v, want %v", spec.Type, reflect.TypeFor[wantProvider]())
	}
	if got := Optional[wantProvider]("provider").Spec(); got.Required {
		t.Fatalf("optional Spec = %+v, want Required=false", got)
	}
}

func TestSpecDepSpecForMatchesManyPrefix(t *testing.T) {
	spec := Spec{Deps: []Requirement{RequireMany[wantProvider]("provider").Spec()}}
	for _, key := range []string{"provider", "provider.openai"} {
		if _, ok := spec.DepSpecFor(key); !ok {
			t.Fatalf("DepSpecFor(%q) = false, want the Many dep", key)
		}
	}
	if _, ok := spec.DepSpecFor("providers"); ok {
		t.Fatal("DepSpecFor matched a key that is not the dep prefix")
	}
}
