package vessel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// SessionStore provisions a per-run [workspace.Workspace] view so
// every agent.Run dispatched by a Captain gets its own filesystem
// boundary that lives for the duration of the run.
//
// Lifecycle contract:
//
//   - Open is called once per run by Captain.submit, right after the
//     admission gate has issued runCtx. Implementations MUST return
//     the same Workspace for repeated Open(runID) calls within a
//     process — that is what lets a checkpoint resume reach the same
//     view it left, and lets in-flight retries through the same runID
//     observe their own prior writes.
//   - Close is called once when the run terminates (success or error).
//     The data underneath the Workspace MAY persist or be discarded;
//     each implementation documents its own retention policy. The
//     Captain uses the *vessel* baseCtx (not the run's ctx) for Close
//     so cleanup survives a runCtx cancellation.
//
// Engines / tools running inside a vessel-dispatched run reach their
// per-run Workspace via [WorkspaceFromContext]. Code outside a
// vessel run (or vessels constructed without [WithSessionStore]) sees
// a nil Workspace + false from that helper, and should fall back to
// caller-supplied workspaces or error out depending on the use case.
type SessionStore interface {
	// Open returns the per-run workspace for runID. Implementations
	// must return the same instance for the same runID within their
	// process lifetime; this is what makes resume / retry idempotent.
	Open(ctx context.Context, runID string) (workspace.Workspace, error)

	// Close releases bookkeeping resources for runID. Whether the
	// underlying data is also deleted is implementation-defined:
	// MemorySessionStore drops the in-memory map entry (so its data
	// is gone), while FilesystemSessionStore preserves the directory
	// on disk (deletion is an operator concern — see its docs).
	// Calling Close on an unknown runID is a no-op.
	Close(ctx context.Context, runID string) error
}

// WorkspaceFromContext extracts the per-run [workspace.Workspace] a
// Captain associated with the current agent.Run. Returns (nil, false)
// when ctx is not a vessel run context or when the Captain was built
// without [WithSessionStore].
//
// The standard pattern for tools / engines that want to be
// session-aware without hard-coding the dependency is:
//
//	if ws, ok := vessel.WorkspaceFromContext(ctx); ok {
//	    // use ws — it lives until the run terminates
//	} else {
//	    // fall back to the tool's own workspace (or refuse)
//	}
//
// This avoids a circular dependency between sdk/tool packages and
// vessel: the helper lives in vessel, callers in sdk import it as a
// downstream dependency.
func WorkspaceFromContext(ctx context.Context) (workspace.Workspace, bool) {
	ws, ok := ctx.Value(sessionCtxKey{}).(workspace.Workspace)
	if !ok || ws == nil {
		return nil, false
	}
	return ws, true
}

// sessionCtxKey is the ctx value-key Captain.submit stashes the
// per-run Workspace under. Private so external code cannot inject a
// counterfeit value — the only way a Workspace reaches the ctx is
// through SessionStore.Open, which is what the security model relies
// on (Open is where path-traversal validation and root scoping
// happen).
type sessionCtxKey struct{}

// MemorySessionStore is the in-process, non-persistent SessionStore:
// every run gets a freshly minted [workspace.MemWorkspace] that lives
// in heap. Close drops the entry; the data goes with it.
//
// Suitable for tests, ephemeral demos, and vessels whose
// CheckpointStore is also in-memory (so a Resume across the loss
// boundary is impossible anyway). For production deployments with a
// persistent CheckpointStore, pair it with FilesystemSessionStore
// instead so Resume can reach the on-disk state the prior run left.
//
// Safe for concurrent Open / Close from multiple Captain goroutines.
type MemorySessionStore struct {
	mu       sync.Mutex
	sessions map[string]workspace.Workspace
}

// NewMemorySessionStore constructs an empty in-memory session store.
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{sessions: make(map[string]workspace.Workspace)}
}

// Open returns (or creates) the in-memory workspace for runID. The
// runID is treated as an opaque key — no path validation is applied
// because no filesystem path is constructed from it.
func (s *MemorySessionStore) Open(_ context.Context, runID string) (workspace.Workspace, error) {
	if runID == "" {
		return nil, errdefs.Validationf("vessel: SessionStore.Open requires a non-empty runID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ws, ok := s.sessions[runID]; ok {
		return ws, nil
	}
	ws := workspace.NewMemWorkspace()
	s.sessions[runID] = ws
	return ws, nil
}

// Close drops the workspace bookkeeping for runID. Because
// MemWorkspace data lives entirely inside the released map entry, the
// data is discarded as soon as the last reference (typically the
// caller's ctx-stashed pointer) goes out of scope. Calling Close on
// an unknown runID is a no-op so retries / double-Close are safe.
func (s *MemorySessionStore) Close(_ context.Context, runID string) error {
	if runID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, runID)
	return nil
}

// FilesystemSessionStore lays each run's workspace out as a
// subdirectory of a root directory:
//
//	<root>/<runID>/
//
// A fresh runID provisions a new subdirectory; reopening the same
// runID returns the same [workspace.LocalWorkspace] view onto it.
// Data is preserved across Close (and across process restarts) so a
// downstream CheckpointStore.Resume can reach the files an
// interrupted run left behind.
//
// runIDs must match [ValidateRunID] so a malicious caller cannot
// escape the root with "../etc/passwd" or "/tmp/foo". The validator
// is intentionally narrower than the engine's runID generator emits
// (`run-<hex>`), so the same allow-list passes for both
// Captain-minted and caller-supplied runIDs while rejecting anything
// that looks like a path operator.
//
// Disk reclamation is OUT of scope for this type. Operators decide
// when to GC the root (cron, retention policy, manual prune). The
// store does not delete data on Close because doing so would defeat
// the Resume use case the persistence layer exists for; callers that
// want ephemeral semantics should pick MemorySessionStore instead.
//
// Safe for concurrent Open / Close from multiple Captain goroutines.
type FilesystemSessionStore struct {
	root     string
	mu       sync.Mutex
	sessions map[string]workspace.Workspace
}

// NewFilesystemSessionStore constructs a FilesystemSessionStore
// rooted at root, creating the directory tree when missing.
//
// The root is resolved through filepath.EvalSymlinks (when the path
// already exists) so a later symlink swap on the root cannot be used
// to escape — same defence LocalWorkspace and sandbox.LocalRunner
// already apply to their own roots.
func NewFilesystemSessionStore(root string) (*FilesystemSessionStore, error) {
	if root == "" {
		return nil, errdefs.Validationf("vessel: FilesystemSessionStore requires a non-empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("vessel: resolve session root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("vessel: create session root: %w", err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = resolved
	}
	return &FilesystemSessionStore{
		root:     abs,
		sessions: make(map[string]workspace.Workspace),
	}, nil
}

// Root reports the absolute, symlink-resolved path the store is
// rooted at. Useful for tests that want to assert layout, and for
// operators wiring retention / GC.
func (s *FilesystemSessionStore) Root() string { return s.root }

// Open returns (or creates) the on-disk workspace for runID. The
// runID is validated by [ValidateRunID] before being used as a path
// component.
func (s *FilesystemSessionStore) Open(_ context.Context, runID string) (workspace.Workspace, error) {
	if err := ValidateRunID(runID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ws, ok := s.sessions[runID]; ok {
		return ws, nil
	}
	dir := filepath.Join(s.root, runID)
	ws, err := workspace.NewLocalWorkspace(dir)
	if err != nil {
		return nil, fmt.Errorf("vessel: open session %q: %w", runID, err)
	}
	s.sessions[runID] = ws
	return ws, nil
}

// Close drops the in-memory bookkeeping for runID. The on-disk
// directory is preserved — see the type-level doc for the rationale.
// Calling Close on an unknown runID is a no-op so retries /
// double-Close are safe.
func (s *FilesystemSessionStore) Close(_ context.Context, runID string) error {
	if runID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, runID)
	return nil
}

// ValidateRunID rejects runIDs that could be abused as path operators
// or escape the FilesystemSessionStore root. Allowed characters are
// ASCII letters, digits, dash, and underscore — a strict subset of
// what filesystems accept, deliberately narrower than RFC 3986 path
// segments to avoid surprises across Linux / macOS / Windows.
//
// Exported so callers building custom SessionStore implementations
// can reuse the same allow-list and stay consistent with the
// filesystem store's contract.
func ValidateRunID(runID string) error {
	if runID == "" {
		return errdefs.Validationf("vessel: SessionStore.Open requires a non-empty runID")
	}
	if runID == "." || runID == ".." {
		return errdefs.Validationf("vessel: runID %q is a reserved path component", runID)
	}
	for _, r := range runID {
		switch {
		case 'a' <= r && r <= 'z':
		case 'A' <= r && r <= 'Z':
		case '0' <= r && r <= '9':
		case r == '-' || r == '_':
		default:
			return errdefs.Validationf(
				"vessel: runID %q contains disallowed character %q (allowed: [A-Za-z0-9_-])",
				runID, r)
		}
	}
	return nil
}
