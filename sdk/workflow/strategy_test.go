package workflow

import "testing"

// --- Dependencies ---

func TestNewDependencies(t *testing.T) {
	d := NewDependencies()
	if d == nil || d.store == nil {
		t.Fatal("expected initialized container")
	}
}

func TestDependencies_SetAndGetDep(t *testing.T) {
	d := NewDependencies()
	SetDep(d, "key", 42)

	v, err := GetDep[int](d, "key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 42 {
		t.Fatalf("expected 42, got %d", v)
	}
}

func TestDependencies_Set(t *testing.T) {
	d := NewDependencies()
	d.Set("k", "v")

	v, err := GetDep[string](d, "k")
	if err != nil {
		t.Fatal(err)
	}
	if v != "v" {
		t.Fatalf("expected 'v', got %q", v)
	}
}

func TestDependencies_Set_NilStore(t *testing.T) {
	d := &Dependencies{}
	d.Set("k", "v")

	v, err := GetDep[string](d, "k")
	if err != nil {
		t.Fatal(err)
	}
	if v != "v" {
		t.Fatalf("expected 'v', got %q", v)
	}
}

func TestGetDep_Missing(t *testing.T) {
	d := NewDependencies()
	_, err := GetDep[int](d, "missing")
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestGetDep_TypeMismatch(t *testing.T) {
	d := NewDependencies()
	SetDep(d, "key", "a string")

	_, err := GetDep[int](d, "key")
	if err == nil {
		t.Fatal("expected error for type mismatch")
	}
}

func TestGetDep_NilDependencies(t *testing.T) {
	var d *Dependencies
	_, err := GetDep[int](d, "key")
	if err == nil {
		t.Fatal("expected error for nil Dependencies")
	}
}

func TestGetDep_NilStore(t *testing.T) {
	d := &Dependencies{}
	_, err := GetDep[int](d, "key")
	if err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestDependencies_Overwrite(t *testing.T) {
	d := NewDependencies()
	SetDep(d, "key", 1)
	SetDep(d, "key", 2)

	v, err := GetDep[int](d, "key")
	if err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("expected overwritten value 2, got %d", v)
	}
}

// --- StrategyCapabilities ---

func TestStrategyCapabilities_AnswerVar_Default(t *testing.T) {
	c := StrategyCapabilities{}
	if c.AnswerVar() != VarAnswer {
		t.Fatalf("expected %q, got %q", VarAnswer, c.AnswerVar())
	}
}

func TestStrategyCapabilities_AnswerVar_Custom(t *testing.T) {
	c := StrategyCapabilities{AnswerKey: "custom_answer"}
	if c.AnswerVar() != "custom_answer" {
		t.Fatalf("expected 'custom_answer', got %q", c.AnswerVar())
	}
}
