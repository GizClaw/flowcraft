package ptr

import "testing"

func TestClone(t *testing.T) {
	if got := Clone[int](nil); got != nil {
		t.Fatalf("Clone(nil) = %v, want nil", got)
	}
	value := 7
	clone := Clone(&value)
	if clone == &value {
		t.Fatal("Clone returned the original pointer")
	}
	if *clone != value {
		t.Fatalf("Clone copied %d, want %d", *clone, value)
	}
	*clone = 9
	if value != 7 {
		t.Fatalf("mutating the clone changed the original: %d", value)
	}
}

func TestIsNil(t *testing.T) {
	var nilPointer *int
	var nilMap map[string]int
	var nilSlice []int
	var nilChan chan int
	var nilFunc func()
	var nilInterface any = (*int)(nil)

	for name, value := range map[string]any{
		"nil":         nil,
		"typed nil":   nilInterface,
		"nil pointer": nilPointer,
		"nil map":     nilMap,
		"nil slice":   nilSlice,
		"nil chan":    nilChan,
		"nil func":    nilFunc,
	} {
		if !IsNil(value) {
			t.Errorf("IsNil(%s) = false, want true", name)
		}
	}

	value := 7
	for name, candidate := range map[string]any{
		"int":         0,
		"string":      "",
		"struct":      struct{}{},
		"pointer":     &value,
		"empty map":   map[string]int{},
		"empty slice": []int{},
	} {
		if IsNil(candidate) {
			t.Errorf("IsNil(%s) = true, want false", name)
		}
	}
}
