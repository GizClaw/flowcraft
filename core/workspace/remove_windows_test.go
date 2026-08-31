//go:build windows

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	xwin "golang.org/x/sys/windows"
)

// TestRemoveWithRetryTransient verifies the retry loop turns a
// transient sharing violation into a success.
func TestRemoveWithRetryTransient(t *testing.T) {
	calls := 0
	err := removeWithRetry(func() error {
		calls++
		if calls < 3 {
			return xwin.ERROR_SHARING_VIOLATION
		}
		return nil
	})
	if err != nil {
		t.Fatalf("removeWithRetry: %v", err)
	}
	if calls != 3 {
		t.Fatalf("op calls = %d, want 3", calls)
	}
}

// TestRemoveWithRetryGivesUp verifies a persistently locked file still
// reports the original error after the bounded retries.
func TestRemoveWithRetryGivesUp(t *testing.T) {
	err := removeWithRetry(func() error { return xwin.ERROR_SHARING_VIOLATION })
	if !errors.Is(err, xwin.ERROR_SHARING_VIOLATION) {
		t.Fatalf("err = %v, want sharing violation", err)
	}
}

// TestRemoveWithRetryNonRetryable verifies non-lock errors pass
// through without retrying.
func TestRemoveWithRetryNonRetryable(t *testing.T) {
	calls := 0
	err := removeWithRetry(func() error {
		calls++
		return os.ErrNotExist
	})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
	if calls != 1 {
		t.Fatalf("op calls = %d, want 1", calls)
	}
}

// TestDeleteLockedFile confirms a file held open without
// FILE_SHARE_DELETE still fails Delete after the bounded retries
// instead of succeeding or hanging.
func TestDeleteLockedFile(t *testing.T) {
	ws, _ := newLocalWS(t)
	path := filepath.Join(ws.Root(), "locked.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = f.Close() }()

	if err := ws.Delete(context.Background(), "locked.txt"); err == nil {
		t.Fatal("Delete succeeded for a file held open without FILE_SHARE_DELETE")
	}
}

// TestRemoveAllLockedFile confirms RemoveAll also surfaces the
// locked-file failure instead of silently losing the error.
func TestRemoveAllLockedFile(t *testing.T) {
	ws, _ := newLocalWS(t)
	if err := os.MkdirAll(filepath.Join(ws.Root(), "tree"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(ws.Root(), "tree", "locked.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = f.Close() }()

	if err := ws.RemoveAll(context.Background(), "tree"); err == nil {
		t.Fatal("RemoveAll succeeded with a locked file inside")
	}
}
