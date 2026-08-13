package deploy

import (
	"errors"
	"testing"
)

type closeErr struct{ err error }

func (c closeErr) Close() error { return c.err }

func TestCloseAllJoinsErrors(t *testing.T) {
	first := errors.New("first")
	second := errors.New("second")
	err := closeAll(
		map[string]any{
			"a": closeErr{err: first},
			"b": closeErr{err: second},
		},
		[]string{"a", "b"},
	)
	if err == nil {
		t.Fatal("closeAll returned nil, want joined errors")
	}
	if !errors.Is(err, first) || !errors.Is(err, second) {
		t.Fatalf("closeAll error = %v, want both first and second", err)
	}
}
