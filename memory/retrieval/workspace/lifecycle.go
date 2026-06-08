package workspace

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Drop implements [retrieval.Droppable]. It removes the namespace root
// from the backing Workspace and evicts any opened state so the next use
// reopens a fresh namespace.
func (idx *Index) Drop(ctx context.Context, namespace string) error {
	if idx.closed.Load() {
		return ErrClosed
	}
	if namespace == "" {
		return errEmptyNamespace
	}
	paths := newPathHelper(namespace)
	if paths.nsDir() == "." {
		return errdefs.Validationf("retrieval/workspace: namespace must not be workspace root")
	}

	idx.nsMu.Lock()
	defer idx.nsMu.Unlock()
	if idx.closed.Load() {
		return ErrClosed
	}

	st := idx.namespaces[namespace]
	if st == nil {
		return idx.ws.RemoveAll(ctx, paths.nsDir())
	}
	if err := fenceCheck(st); err != nil {
		return err
	}
	delete(idx.namespaces, namespace)
	st.retired.Store(true)

	// Drain namespace-local background work before deleting its root.
	// Holding nsMu prevents a fresh open of this namespace until the
	// RemoveAll below has completed.
	if st.lockCancel != nil {
		st.lockCancel()
		<-st.lockDone
		st.lockCancel = nil
		st.lockDone = nil
	}

	st.compactMu.Lock()
	defer st.compactMu.Unlock()

	st.rwMu.Lock()
	if st.wal != nil {
		_ = st.wal.Close()
		st.wal = nil
	}
	st.rwMu.Unlock()

	idx.releaseLock(ctx, st)
	dropPath := st.paths.nsDir()
	if dropPath == "" {
		dropPath = paths.nsDir()
	}
	if dropPath == "." {
		return errdefs.Validationf("retrieval/workspace: namespace must not be workspace root")
	}
	return idx.ws.RemoveAll(ctx, dropPath)
}
