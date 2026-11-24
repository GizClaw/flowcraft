package ptr

import "testing"

func TestCloneIsNilForNil(t *testing.T) {
	var value *int
	if got := Clone(value); got != nil {
		t.Fatalf("Clone(nil) = %v, want nil", got)
	}
}

// TestCloneDoesNotAlias is the whole point of the helper: the caller keeps the
// original and hands the copy away.
func TestCloneDoesNotAlias(t *testing.T) {
	original := 42
	cloned := Clone(&original)
	if cloned == nil || *cloned != 42 {
		t.Fatalf("Clone = %v, want a copy of 42", cloned)
	}
	if cloned == &original {
		t.Fatal("Clone returned the original pointer")
	}
	original = 7
	if *cloned != 42 {
		t.Fatalf("mutating the original changed the clone to %d", *cloned)
	}
}

// TestIsNilSeesThroughAnInterface is the case that motivates the helper: a typed
// nil boxed in an any is not nil by comparison — `boxed == nil` is false, and the
// compiler knows it — so a caller that checks `== nil` misses exactly the value it
// was trying to catch.
func TestIsNilSeesThroughAnInterface(t *testing.T) {
	var missing *int
	var boxed any = missing

	if !IsNil(boxed) {
		t.Fatal("IsNil missed a typed nil behind an interface")
	}
	if !IsNil(nil) {
		t.Fatal("IsNil(nil) = false")
	}
}

func TestIsNilCoversTheNilableKinds(t *testing.T) {
	for name, value := range map[string]any{
		"map":       map[string]int(nil),
		"slice":     []int(nil),
		"chan":      (chan int)(nil),
		"func":      (func())(nil),
		"interface": any(nil),
	} {
		t.Run(name, func(t *testing.T) {
			if !IsNil(value) {
				t.Fatalf("IsNil(%s) = false", name)
			}
		})
	}
}

// A value of a non-nilable kind is never nil, whatever reflect says about the
// zero value.
func TestIsNilIsFalseForValues(t *testing.T) {
	for name, value := range map[string]any{
		"int":    0,
		"string": "",
		"struct": struct{}{},
		"array":  [2]int{},
	} {
		t.Run(name, func(t *testing.T) {
			if IsNil(value) {
				t.Fatalf("IsNil(%s) = true", name)
			}
		})
	}
}
