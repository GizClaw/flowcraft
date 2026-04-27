package engine_test

import (
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

type stubLLM struct{ name string }

type llmKey struct{}

func TestDependencies_SetGet(t *testing.T) {
	d := engine.NewDependencies()
	d.Set(llmKey{}, &stubLLM{name: "x"})

	v, ok := d.Get(llmKey{})
	if !ok {
		t.Fatal("Get must report ok for set key")
	}
	llm, ok := v.(*stubLLM)
	if !ok {
		t.Fatalf("value type = %T, want *stubLLM", v)
	}
	if llm.name != "x" {
		t.Errorf("name = %q, want x", llm.name)
	}
}

func TestDependencies_Has(t *testing.T) {
	d := engine.NewDependencies()
	d.Set("k", "v")

	if !d.Has("k") {
		t.Error("Has must report true for set key")
	}
	if d.Has("missing") {
		t.Error("Has must report false for unset key")
	}
}

func TestDependencies_Remove(t *testing.T) {
	d := engine.NewDependencies()
	d.Set("k", "v")
	d.Remove("k")
	if d.Has("k") {
		t.Error("Remove did not delete the key")
	}
	d.Remove("missing")
}

func TestDependencies_Get_NilReceiverReturnsFalse(t *testing.T) {
	var d *engine.Dependencies
	if _, ok := d.Get("k"); ok {
		t.Error("Get on nil deps must report ok=false")
	}
	if d.Has("k") {
		t.Error("Has on nil deps must report false")
	}
}

func TestGetDep_NilDepsReportsError(t *testing.T) {
	var d *engine.Dependencies
	_, err := engine.GetDep[string](d, "k")
	if err == nil {
		t.Error("GetDep on nil deps must error")
	}
}

func TestGetDep_MissingKeyReportsError(t *testing.T) {
	d := engine.NewDependencies()
	_, err := engine.GetDep[string](d, "absent")
	if err == nil {
		t.Error("GetDep on missing key must error")
	}
}

func TestGetDep_TypeMismatchReportsError(t *testing.T) {
	d := engine.NewDependencies()
	d.Set("k", 42)

	_, err := engine.GetDep[string](d, "k")
	if err == nil {
		t.Error("GetDep with wrong type must error")
	}
}

func TestGetDep_HappyPath(t *testing.T) {
	d := engine.NewDependencies()
	d.Set(llmKey{}, &stubLLM{name: "y"})

	got, err := engine.GetDep[*stubLLM](d, llmKey{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.name != "y" {
		t.Errorf("name = %q, want y", got.name)
	}
}

func TestMustGetDep_PanicsOnError(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Error("MustGetDep should panic on missing key")
			return
		}
		err, ok := r.(error)
		if !ok {
			t.Errorf("recovered value type = %T, want error", r)
			return
		}
		if !errorContains(err, "not found") {
			t.Errorf("panic error = %v, want one mentioning 'not found'", err)
		}
	}()
	engine.MustGetDep[string](engine.NewDependencies(), "absent")
}

func errorContains(err error, sub string) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, err) && contains(err.Error(), sub)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
