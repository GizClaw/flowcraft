package workspace

import (
	"context"
	"testing"
	"time"

	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestEnsureNamespaceWaitsForInFlightInitialization(t *testing.T) {
	ctx := context.Background()
	idx, err := New(sdkworkspace.NewMemWorkspace(), WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	st := &namespaceState{name: "ns"}
	st.rwMu.Lock()
	idx.nsMu.Lock()
	idx.namespaces["ns"] = st
	idx.nsMu.Unlock()

	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		_, err := idx.ensureNamespace(ctx, "ns")
		errCh <- err
		close(done)
	}()

	select {
	case err := <-errCh:
		t.Fatalf("ensureNamespace returned before initialization completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	st.manifest = &manifest{Version: manifestVersion}
	st.memtable = newMemtable()
	st.rwMu.Unlock()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ensureNamespace after initialization error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ensureNamespace did not resume after initialization completed")
	}

	<-done
}
