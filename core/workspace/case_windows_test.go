//go:build windows

package workspace

import (
	"context"
	"errors"
	"testing"
)

// Case-insensitive path semantics: Windows filesystems fold case, so
// scoped deny/allow rules and glob patterns must match case variants
// the way the filesystem does. A case-sensitive match here would let a
// deny-read rule be bypassed by a different-cased path.

func TestScopedWorkspaceCaseInsensitiveDenyRead(t *testing.T) {
	ctx := context.Background()
	inner := newTestWorkspace(t)
	if err := inner.Write(ctx, "Secret/key.txt", []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sw := NewScopedWorkspace(inner, WithDenyRead("secret/**"))
	if _, err := sw.Read(ctx, "Secret/key.txt"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("case-variant read err = %v, want ErrAccessDenied", err)
	}
}

func TestScopedWorkspaceCaseInsensitiveMandatoryDeny(t *testing.T) {
	ctx := context.Background()
	inner := newTestWorkspace(t)
	if err := inner.Write(ctx, "Config/app.yaml", []byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sw := NewScopedWorkspace(inner, WithMandatoryDeny("config/**"))
	if _, err := sw.Read(ctx, "Config/app.yaml"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("case-variant read err = %v, want ErrAccessDenied", err)
	}
}

func TestScopedWorkspaceCaseInsensitiveAllowWrite(t *testing.T) {
	ctx := context.Background()
	inner := newTestWorkspace(t)
	sw := NewScopedWorkspace(inner, WithAllowWrite("public/**"))
	if err := sw.Write(ctx, "Public/ok.txt", []byte("x")); err != nil {
		t.Fatalf("case-variant write err = %v, want allowed", err)
	}
	if _, err := inner.Read(ctx, "Public/ok.txt"); err != nil {
		t.Fatalf("read back: %v", err)
	}
}

func TestGlobCaseInsensitive(t *testing.T) {
	ws := newTestWorkspace(t)
	mustWrite(t, ws, "src/DATA.JSON", []byte("x"))
	matches, err := Glob(context.Background(), ws, "src/*.json")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %v, want 1 case-insensitive match", matches)
	}
}
