package resolver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/vessel"
)

func TestResolve_SessionStore_Memory(t *testing.T) {
	t.Parallel()
	doc := `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: d
spec:
  control:
    socket: /tmp/vesseld.sock
  sessionStore:
    backend: memory
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: v
spec:
  agents: [a]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: a
spec:
  engine:
    ref: noop-ss
`
	plan := resolveOK(t, doc)
	if plan.SharedSessionStore == nil {
		t.Fatal("SharedSessionStore is nil")
	}
	if _, ok := plan.SharedSessionStore.(*vessel.MemorySessionStore); !ok {
		t.Fatalf("want *MemorySessionStore, got %T", plan.SharedSessionStore)
	}
}

func TestResolve_SessionStore_Filesystem(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "sessions")
	doc := `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: d
spec:
  control:
    socket: /tmp/vesseld.sock
  sessionStore:
    backend: filesystem
    root: ` + root + `
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: v
spec:
  agents: [a]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: a
spec:
  engine:
    ref: noop-ss
`
	plan := resolveOK(t, doc)
	fs, ok := plan.SharedSessionStore.(*vessel.FilesystemSessionStore)
	if !ok {
		t.Fatalf("want *FilesystemSessionStore, got %T", plan.SharedSessionStore)
	}
	// macOS resolves /var → /private/var symlinks inside Abs(),
	// so compare via samefile semantics instead of string equality.
	if !sameDir(t, fs.Root(), root) {
		t.Fatalf("Root() = %q, want %q (or its symlink-resolved equivalent)", fs.Root(), root)
	}
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	sa, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	sb, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(sa, sb)
}

func TestResolve_SessionStore_Omitted_Nil(t *testing.T) {
	t.Parallel()
	doc := `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: d
spec:
  control:
    socket: /tmp/vesseld.sock
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: v
spec:
  agents: [a]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: a
spec:
  engine:
    ref: noop-ss
`
	plan := resolveOK(t, doc)
	if plan.SharedSessionStore != nil {
		t.Fatalf("SharedSessionStore = %T, want nil", plan.SharedSessionStore)
	}
}

func TestBuildSessionStore_UnknownBackend(t *testing.T) {
	t.Parallel()
	// Exercises the defensive default branch directly. The
	// apispec validator catches this before resolver runs in the
	// happy path, but if a caller skips Validate we still want
	// a structured error, not a panic.
	_, err := buildSessionStore(&v1alpha1.DaemonSessionStore{Backend: "redis"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

// resolveOK is a small helper that resolves doc with a no-op
// engine factory and fails the test on any error. Used by the
// session-store tests so each case stays focused on the
// SharedSessionStore field.
func resolveOK(t *testing.T, doc string) *Plan {
	t.Helper()
	objs, err := apispec.DecodeAll(strings.NewReader(doc), "<test>")
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	cat := catalog.New()
	cat.RegisterEngine("noop-ss", func(_ string, _ map[string]any, _ catalog.Deps) (engine.Engine, error) {
		return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			return b, nil
		}), nil
	})
	plan, errs := Resolve(objs, cat, ResolveOptions{})
	if errs.Len() != 0 {
		t.Fatalf("Resolve errors: %v", errs)
	}
	return plan
}
