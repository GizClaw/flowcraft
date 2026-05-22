package workspace_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	wsindex "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestLock_SecondAcquireOnLiveLockReturnsErrLocked(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	ctx := context.Background()

	idx1, err := wsindex.New(ws,
		wsindex.WithAutoCompact(false),
		wsindex.WithLockHeartbeat(50*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx1.Close() })
	if err := idx1.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a", Content: "alpha"}}); err != nil {
		t.Fatal(err)
	}

	// A second Index over the same workspace should observe the
	// live lockfile and refuse to acquire the same namespace.
	idx2, err := wsindex.New(ws,
		wsindex.WithAutoCompact(false),
		wsindex.WithLockHeartbeat(50*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx2.Close() })
	err = idx2.Upsert(ctx, "ns", []retrieval.Doc{{ID: "b", Content: "beta"}})
	if err == nil {
		t.Fatal("expected ErrLocked, got nil")
	}
	if !errdefs.IsConflict(err) {
		t.Errorf("err category = %v, want Conflict", err)
	}
}

func TestLock_StaleLockIsTakenOver(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	ctx := context.Background()

	// Hand-write a stale lockfile (HeartbeatAt 1 hour ago) so the
	// next acquirer's staleness check trips.
	stale := map[string]any{
		"version":      1,
		"holder":       "ghost",
		"pid":          99999,
		"acquired_at":  time.Now().Add(-time.Hour),
		"heartbeat_at": time.Now().Add(-time.Hour),
	}
	raw, _ := json.Marshal(stale)
	if err := ws.Write(ctx, "ns/.lock", raw); err != nil {
		t.Fatal(err)
	}

	idx, err := wsindex.New(ws,
		wsindex.WithAutoCompact(false),
		wsindex.WithLockHeartbeat(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a"}}); err != nil {
		t.Errorf("expected stale takeover, got %v", err)
	}
}

func TestLock_HeartbeatRefreshesFile(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	ctx := context.Background()

	idx, err := wsindex.New(ws,
		wsindex.WithAutoCompact(false),
		wsindex.WithLockHeartbeat(40*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	// Force ns open via an upsert so the heartbeat goroutine starts.
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}

	first := readLock(t, ws, "ns")
	time.Sleep(150 * time.Millisecond)
	second := readLock(t, ws, "ns")
	if !second.HeartbeatAt.After(first.HeartbeatAt) {
		t.Errorf("heartbeat did not advance: first=%v second=%v",
			first.HeartbeatAt, second.HeartbeatAt)
	}
	if second.Holder != first.Holder {
		t.Errorf("holder changed unexpectedly: %s -> %s", first.Holder, second.Holder)
	}
}

func TestLock_FencedAfterPeerTakeover(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	ctx := context.Background()

	idx, err := wsindex.New(ws,
		wsindex.WithAutoCompact(false),
		wsindex.WithLockHeartbeat(40*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}

	// Simulate a peer takeover: overwrite the lockfile with a new
	// holder. The heartbeat loop's next tick will read this,
	// notice the holder mismatch, and trip fenced.
	peer := map[string]any{
		"version":      1,
		"holder":       "peer-takeover",
		"pid":          77777,
		"acquired_at":  time.Now(),
		"heartbeat_at": time.Now(),
	}
	raw, _ := json.Marshal(peer)
	if err := ws.Write(ctx, "ns/.lock", raw); err != nil {
		t.Fatal(err)
	}

	// Wait for the heartbeat tick to detect the fence.
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "b"}})
		if err != nil && errors.Is(err, wsindex.ErrFenced) {
			lastErr = err
			break
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	if !errors.Is(lastErr, wsindex.ErrFenced) {
		t.Fatalf("never observed ErrFenced; lastErr=%v", lastErr)
	}
	if !errdefs.IsNotAvailable(lastErr) {
		t.Errorf("ErrFenced category = %v, want NotAvailable", lastErr)
	}
}

func TestLock_CloseDoesNotEraseOtherHolder(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	ctx := context.Background()

	idx, err := wsindex.New(ws,
		wsindex.WithAutoCompact(false),
		wsindex.WithLockHeartbeat(40*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}
	// Overwrite with a peer holder.
	peer := map[string]any{
		"version":      1,
		"holder":       "new-holder",
		"pid":          77777,
		"acquired_at":  time.Now(),
		"heartbeat_at": time.Now(),
	}
	raw, _ := json.Marshal(peer)
	if err := ws.Write(ctx, "ns/.lock", raw); err != nil {
		t.Fatal(err)
	}

	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	// The lockfile must still name "new-holder" — the closing
	// (fenced) Index must not delete a peer's lock.
	st := readLock(t, ws, "ns")
	if st.Holder != "new-holder" {
		t.Errorf("Close erased peer's lockfile; current holder=%q", st.Holder)
	}
}

func TestLock_ReleaseOnCleanClose(t *testing.T) {
	ws := sdkworkspace.NewMemWorkspace()
	ctx := context.Background()

	idx, err := wsindex.New(ws,
		wsindex.WithAutoCompact(false),
		wsindex.WithLockHeartbeat(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, "ns", []retrieval.Doc{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	exists, err := ws.Exists(ctx, "ns/.lock")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Errorf("clean Close should remove lockfile but it still exists")
	}
}

// readLock decodes the namespace's lockfile or fails the test.
func readLock(t *testing.T, ws sdkworkspace.Workspace, ns string) lockSnapshot {
	t.Helper()
	raw, err := ws.Read(context.Background(), ns+"/.lock")
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	var st lockSnapshot
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("unmarshal lock: %v", err)
	}
	return st
}

// lockSnapshot mirrors the package-private lockState struct for
// black-box test inspection. Keeping a shadow copy avoids exporting
// the real struct.
type lockSnapshot struct {
	Version     int       `json:"version"`
	Holder      string    `json:"holder"`
	PID         int       `json:"pid"`
	Hostname    string    `json:"hostname"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}
