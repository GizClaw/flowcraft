package flowcraft

import (
	"errors"
	"testing"
)

func TestNewRequiresRetrievalIndex(t *testing.T) {
	_, err := New(Options{Name: "flowcraft-recall-v1"})
	if !errors.Is(err, ErrRetrievalIndexRequired) {
		t.Fatalf("New error = %v, want %v", err, ErrRetrievalIndexRequired)
	}
}
