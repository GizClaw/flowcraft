package model

import "testing"

func TestClonePointer(t *testing.T) {
	if got := ClonePointer[int](nil); got != nil {
		t.Fatalf("ClonePointer(nil) = %v, want nil", got)
	}
	value := 7
	clone := ClonePointer(&value)
	if clone == &value {
		t.Fatal("ClonePointer returned the original pointer")
	}
	if *clone != value {
		t.Fatalf("ClonePointer copied %d, want %d", *clone, value)
	}
	*clone = 9
	if value != 7 {
		t.Fatalf("mutating the clone changed the original: %d", value)
	}
}
