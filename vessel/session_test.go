package vessel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// ---------------------------------------------------------------------------
// ValidateRunID
// ---------------------------------------------------------------------------

func TestValidateRunID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"happy_hex", "run-a1b2c3d4", false},
		{"happy_uuid_like", "01HZA9F-9KQ_PVTM", false},
		{"alnum_only", "abc123", false},
		{"empty", "", true},
		{"single_dot", ".", true},
		{"double_dot", "..", true},
		{"contains_slash", "run/abc", true},
		{"contains_backslash", `run\abc`, true},
		{"contains_dotdot_seq", "..foo", true},
		{"contains_space", "run abc", true},
		{"contains_unicode", "run-übung", true},
		{"contains_colon", "run:abc", true},
		{"contains_tilde", "run~abc", true},
		{"leading_dash_ok", "-abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRunID(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", tc.in)
				} else if !errdefs.IsValidation(err) {
					t.Errorf("expected Validation, got %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error for %q: %v", tc.in, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MemorySessionStore
// ---------------------------------------------------------------------------

func TestMemorySessionStore_OpenReturnsSameInstance(t *testing.T) {
	t.Parallel()
	s := NewMemorySessionStore()
	ctx := context.Background()

	a, err := s.Open(ctx, "run-1")
	if err != nil {
		t.Fatalf("Open#1: %v", err)
	}
	b, err := s.Open(ctx, "run-1")
	if err != nil {
		t.Fatalf("Open#2: %v", err)
	}
	if a != b {
		t.Errorf("expected the same workspace for the same runID; got two different instances")
	}

	c, err := s.Open(ctx, "run-2")
	if err != nil {
		t.Fatalf("Open#3: %v", err)
	}
	if a == c {
		t.Errorf("expected distinct workspaces for distinct runIDs")
	}
}

func TestMemorySessionStore_CloseDropsState(t *testing.T) {
	t.Parallel()
	s := NewMemorySessionStore()
	ctx := context.Background()

	first, err := s.Open(ctx, "run-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.Write(ctx, "k", []byte("v")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := s.Close(ctx, "run-1"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-Open after Close: must mint a fresh workspace (no data
	// carry-over). MemorySessionStore is explicitly the ephemeral
	// variant, so dropping state is the documented contract.
	second, err := s.Open(ctx, "run-1")
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	if first == second {
		t.Errorf("expected fresh workspace after Close, got same instance")
	}
	if got, err := second.Exists(ctx, "k"); err != nil {
		t.Fatalf("Exists: %v", err)
	} else if got {
		t.Errorf("expected dropped state, key %q still visible", "k")
	}
}

func TestMemorySessionStore_RejectsEmptyRunID(t *testing.T) {
	t.Parallel()
	s := NewMemorySessionStore()
	_, err := s.Open(context.Background(), "")
	if err == nil || !errdefs.IsValidation(err) {
		t.Errorf("expected Validation for empty runID, got %v", err)
	}
}

func TestMemorySessionStore_CloseUnknownIsNoop(t *testing.T) {
	t.Parallel()
	s := NewMemorySessionStore()
	if err := s.Close(context.Background(), "never-opened"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := s.Close(context.Background(), ""); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMemorySessionStore_ConcurrentOpen(t *testing.T) {
	t.Parallel()
	s := NewMemorySessionStore()
	ctx := context.Background()

	var wg sync.WaitGroup
	const n = 50
	got := make([]workspace.Workspace, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws, err := s.Open(ctx, "shared")
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			got[i] = ws
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if got[i] != got[0] {
			t.Errorf("concurrent Open returned different instances for the same runID")
			break
		}
	}
}

// ---------------------------------------------------------------------------
// FilesystemSessionStore
// ---------------------------------------------------------------------------

func TestFilesystemSessionStore_Provisioning(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, err := NewFilesystemSessionStore(root)
	if err != nil {
		t.Fatalf("NewFilesystemSessionStore: %v", err)
	}

	ctx := context.Background()
	ws, err := s.Open(ctx, "run-abc")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := ws.Write(ctx, "hello.txt", []byte("world")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// File MUST exist under <root>/<runID>/.
	want := filepath.Join(s.Root(), "run-abc", "hello.txt")
	if data, err := os.ReadFile(want); err != nil {
		t.Fatalf("expected on-disk file at %s: %v", want, err)
	} else if string(data) != "world" {
		t.Errorf("unexpected contents %q", data)
	}
}

func TestFilesystemSessionStore_DataSurvivesClose(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, err := NewFilesystemSessionStore(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	ws, _ := s.Open(ctx, "run-x")
	if err := ws.Write(ctx, "state.json", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Close(ctx, "run-x"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-Open returns the same data — this is the Resume contract
	// that distinguishes FilesystemSessionStore from MemorySessionStore.
	again, err := s.Open(ctx, "run-x")
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	data, err := again.Read(ctx, "state.json")
	if err != nil {
		t.Fatalf("Read after Close: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("unexpected contents %q", data)
	}
}

func TestFilesystemSessionStore_RejectsRunIDEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, err := NewFilesystemSessionStore(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	for _, bad := range []string{"..", "../escape", "run/sub", "run\\sub", ""} {
		_, err := s.Open(ctx, bad)
		if err == nil || !errdefs.IsValidation(err) {
			t.Errorf("expected Validation for %q, got %v", bad, err)
		}
	}

	// Confirm no escape attempts created anything outside root.
	if entries, err := os.ReadDir(filepath.Dir(root)); err == nil {
		for _, e := range entries {
			if e.Name() == "escape" {
				t.Errorf("escape directory created outside root: %s", e.Name())
			}
		}
	}
}

func TestFilesystemSessionStore_OpenReturnsSameInstance(t *testing.T) {
	t.Parallel()
	s, err := NewFilesystemSessionStore(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	a, _ := s.Open(ctx, "run-y")
	b, _ := s.Open(ctx, "run-y")
	if a != b {
		t.Errorf("expected same instance across repeated Open calls")
	}
}

func TestNewFilesystemSessionStore_EmptyRoot(t *testing.T) {
	t.Parallel()
	_, err := NewFilesystemSessionStore("")
	if err == nil || !errdefs.IsValidation(err) {
		t.Errorf("expected Validation for empty root, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// WorkspaceFromContext
// ---------------------------------------------------------------------------

func TestWorkspaceFromContext_AbsentReturnsFalse(t *testing.T) {
	t.Parallel()
	if ws, ok := WorkspaceFromContext(context.Background()); ok {
		t.Errorf("expected (nil, false), got (%v, %v)", ws, ok)
	}
}

func TestWorkspaceFromContext_NilStashReturnsFalse(t *testing.T) {
	t.Parallel()
	// A nil workspace in the value slot must surface as "no
	// session" rather than panicking the caller.
	ctx := context.WithValue(context.Background(), sessionCtxKey{}, workspace.Workspace(nil))
	if ws, ok := WorkspaceFromContext(ctx); ok || ws != nil {
		t.Errorf("expected (nil, false) for nil stash, got (%v, %v)", ws, ok)
	}
}

// ---------------------------------------------------------------------------
// Captain integration
// ---------------------------------------------------------------------------

func newSessionTestCaptain(t *testing.T, eng engine.Engine, opts ...Option) *Captain {
	t.Helper()
	vs := spec.Spec{Agents: []spec.Agent{{Name: "p"}}}
	c, err := New(vs, append([]Option{WithEngine(eng)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Stop(ctx)
	})
	return c
}

// TestCaptain_SessionStoreExposesWorkspaceToEngine asserts that an
// engine running inside a vessel with WithSessionStore sees a
// non-nil workspace via WorkspaceFromContext, and that writes
// through that workspace land on the SessionStore-managed view.
func TestCaptain_SessionStoreExposesWorkspaceToEngine(t *testing.T) {
	t.Parallel()
	store := NewMemorySessionStore()

	var (
		mu        sync.Mutex
		seenWS    workspace.Workspace
		seenOK    bool
		seenRunID string
	)
	eng := engine.EngineFunc(func(ctx context.Context, run engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		ws, ok := WorkspaceFromContext(ctx)
		mu.Lock()
		seenWS = ws
		seenOK = ok
		seenRunID = run.ID
		mu.Unlock()
		if ok {
			if err := ws.Write(ctx, "out.txt", []byte("from-engine")); err != nil {
				return b, err
			}
		}
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "done"))
		return b, nil
	})

	c := newSessionTestCaptain(t, eng, WithSessionStore(store))

	h, err := c.Submit(context.Background(), "p", agent.Request{
		RunID:   "run-cap-1",
		Message: model.NewTextMessage(model.RoleUser, "go"),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !seenOK {
		t.Fatalf("engine did not observe a workspace via WorkspaceFromContext")
	}
	if seenWS == nil {
		t.Fatalf("workspace was nil despite ok=true")
	}
	if seenRunID != "run-cap-1" {
		t.Errorf("unexpected runID seen by engine: %q", seenRunID)
	}

	// After the run terminates, store.Close drops bookkeeping.
	// Re-Open returns a fresh workspace (Memory variant) — so the
	// "out.txt" the engine wrote is intentionally gone, demonstrating
	// the ephemeral semantic. Other tests cover the persistent variant.
	ws, err := store.Open(context.Background(), "run-cap-1")
	if err != nil {
		t.Fatalf("post-run Open: %v", err)
	}
	if got, _ := ws.Exists(context.Background(), "out.txt"); got {
		t.Errorf("expected Close to drop in-memory state, but out.txt still visible")
	}
}

// TestCaptain_WithoutSessionStoreLeavesContextEmpty asserts the
// opposite default: with no WithSessionStore, runs see no workspace.
func TestCaptain_WithoutSessionStoreLeavesContextEmpty(t *testing.T) {
	t.Parallel()
	var saw bool
	eng := engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		_, saw = WorkspaceFromContext(ctx)
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "done"))
		return b, nil
	})

	c := newSessionTestCaptain(t, eng)
	h, err := c.Submit(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "go")})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if saw {
		t.Errorf("engine should NOT see a workspace when no SessionStore is wired")
	}
}

// TestCaptain_SessionStoreOpenFailureSurfaces asserts that a Store
// that refuses to provision (e.g. quota exhausted, disk full) fails
// the run cleanly with an Internal error instead of silently
// proceeding without a workspace.
func TestCaptain_SessionStoreOpenFailureSurfaces(t *testing.T) {
	t.Parallel()
	store := &failingStore{}
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		t.Errorf("engine should not run when SessionStore.Open fails")
		return b, nil
	})

	c := newSessionTestCaptain(t, eng, WithSessionStore(store))
	h, err := c.Submit(context.Background(), "p", agent.Request{
		RunID:   "run-fail",
		Message: model.NewTextMessage(model.RoleUser, "go"),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	_, err = h.Wait(context.Background())
	if err == nil {
		t.Fatalf("expected error from failing SessionStore, got nil")
	}
	if !errdefs.IsInternal(err) {
		t.Errorf("expected Internal classification, got %v", err)
	}
	if !strings.Contains(err.Error(), "open session") {
		t.Errorf("error should mention 'open session', got: %v", err)
	}
}

// TestCaptain_SessionStoreCloseAfterTerminate asserts the Captain
// drops the session bookkeeping after the run terminates. We verify
// it by observing that the store's per-run map is empty post-Wait.
func TestCaptain_SessionStoreCloseAfterTerminate(t *testing.T) {
	t.Parallel()
	store := NewMemorySessionStore()
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "done"))
		return b, nil
	})
	c := newSessionTestCaptain(t, eng, WithSessionStore(store))

	h, err := c.Submit(context.Background(), "p", agent.Request{
		RunID:   "run-close",
		Message: model.NewTextMessage(model.RoleUser, "go"),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.sessions["run-close"]; exists {
		t.Errorf("expected store bookkeeping for run-close to be dropped after run terminated")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type failingStore struct{}

func (f *failingStore) Open(_ context.Context, _ string) (workspace.Workspace, error) {
	return nil, errdefs.Internalf("test: open denied")
}

func (f *failingStore) Close(_ context.Context, _ string) error { return nil }
