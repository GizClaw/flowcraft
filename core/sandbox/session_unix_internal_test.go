//go:build unix

package sandbox

import (
	"os"
	"testing"
)

// TestLocalSessionCloseInputIgnoresAlreadyClosed covers the race where
// cmd.Wait (running in reap) closes the parent-side stdin write end just
// before Exec calls CloseInput: closing an already-closed pipe must be a
// no-op success, not an error that fails the whole Exec.
func TestLocalSessionCloseInputIgnoresAlreadyClosed(t *testing.T) {
	_, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	s := &localSession{stdin: w}
	if err := s.CloseInput(); err != nil {
		t.Fatalf("CloseInput on an already-closed stdin = %v, want nil", err)
	}
	if s.stdin != nil {
		t.Fatal("CloseInput should detach the closed stdin")
	}
}
