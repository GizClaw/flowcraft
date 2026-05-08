//go:build e2e

package retrieval_e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	wsindex "github.com/GizClaw/flowcraft/sdkx/retrieval/workspace"
)

// E2E tests against a real on-disk LocalWorkspace.
//
// Every test in this file uses [t.TempDir] for the workspace root,
// so they can run in parallel without contention and clean up
// automatically. The tests assert behaviour that is NOT visible
// from MemWorkspace — atomic Rename actually hitting the disk, real
// CRC of bytes that survived a process boundary, real Lockfile mtime,
// and real RemoveAll deleting directories.

// newLocalIdx builds an Index rooted at a fresh tmp directory. The
// returned cleanup helper closes the Index and reports any error;
// the dir itself is GC'd by t.TempDir.
func newLocalIdx(t *testing.T, opts ...wsindex.Option) (*wsindex.Index, sdkworkspace.Workspace, string) {
	t.Helper()
	dir := t.TempDir()
	ws, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	full := append([]wsindex.Option{
		// Tight heartbeat so cross-process tests converge fast.
		wsindex.WithLockHeartbeat(80 * time.Millisecond),
	}, opts...)
	idx, err := wsindex.New(ws, full...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx, ws, dir
}

func TestLocalE2E_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ws, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Phase 1: write, flush, close.
	{
		idx, err := wsindex.New(ws, wsindex.WithAutoCompact(false))
		if err != nil {
			t.Fatal(err)
		}
		docs := []retrieval.Doc{
			{ID: "a", Content: "the quick brown fox", Metadata: map[string]any{"k": "v"}},
			{ID: "b", Content: "lorem ipsum dolor sit amet"},
		}
		if err := idx.Upsert(ctx, "ns", docs); err != nil {
			t.Fatal(err)
		}
		if err := idx.Flush(ctx, "ns"); err != nil {
			t.Fatal(err)
		}
		if err := idx.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 2: reopen the SAME directory through a fresh Index.
	// The manifest + segment must be readable from disk; Search
	// must return the doc indexed in phase 1.
	ws2, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx2, err := wsindex.New(ws2, wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	resp, err := idx2.Search(ctx, "ns", retrieval.SearchRequest{
		QueryText: "fox",
		TopK:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) == 0 || resp.Hits[0].Doc.ID != "a" {
		t.Errorf("post-restart search did not surface 'a': %+v", resp.Hits)
	}
}

func TestLocalE2E_WALReplayAfterUngracefulShutdown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()

	// Phase 1: write WITHOUT flush; the doc lives only in WAL +
	// memtable. Close (which by contract does NOT flush) drops the
	// in-memory state but leaves the WAL on disk — the protocol
	// promises this is recoverable.
	{
		ws, err := sdkworkspace.NewLocalWorkspace(dir)
		if err != nil {
			t.Fatal(err)
		}
		idx, err := wsindex.New(ws, wsindex.WithAutoCompact(false))
		if err != nil {
			t.Fatal(err)
		}
		if err := idx.Upsert(ctx, "ns",
			[]retrieval.Doc{{ID: "wal-only", Content: "alpha bravo charlie"}}); err != nil {
			t.Fatal(err)
		}
		if err := idx.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Phase 2: reopen — WAL replay should rehydrate the memtable.
	ws2, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx2, err := wsindex.New(ws2, wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	resp, err := idx2.Search(ctx, "ns", retrieval.SearchRequest{
		QueryText: "alpha",
		TopK:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) == 0 || resp.Hits[0].Doc.ID != "wal-only" {
		t.Errorf("WAL replay missed: %+v", resp.Hits)
	}
}

func TestLocalE2E_DiskCorruptionDetected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ws, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := wsindex.New(ws, wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := idx.Upsert(ctx, "ns",
		[]retrieval.Doc{{ID: "x", Content: "hello world"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Flush(ctx, "ns"); err != nil {
		t.Fatal(err)
	}

	// Corrupt the segment's docs.jsonl directly via the OS API,
	// bypassing the workspace abstraction.
	docs, err := findFirstFile(t, filepath.Join(dir, "ns", "segments"), "docs.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 5 {
		raw[5] ^= 0xFF
	}
	if err := os.WriteFile(docs, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// Search must observe the CRC mismatch and refuse rather than
	// silently feed garbled bytes into the BM25 path.
	_, err = idx.Search(ctx, "ns", retrieval.SearchRequest{
		QueryText: "hello",
		TopK:      5,
	})
	if err == nil {
		t.Fatal("expected ErrCorrupt, got nil")
	}
	if !errdefs.IsInternal(err) {
		t.Errorf("err category = %v, want Internal (ErrCorrupt)", err)
	}
	_ = idx.Close()
}

func TestLocalE2E_CompactionRemovesSegmentDirsOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ws, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := wsindex.New(ws,
		wsindex.WithAutoCompact(false),
		wsindex.WithCompactionMinSegments(3),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ctx := context.Background()

	// Five flushes -> five segments.
	for i := 0; i < 5; i++ {
		if err := idx.Upsert(ctx, "ns",
			[]retrieval.Doc{{ID: string(rune('a' + i)), Content: "fox"}}); err != nil {
			t.Fatal(err)
		}
		if err := idx.Flush(ctx, "ns"); err != nil {
			t.Fatal(err)
		}
	}
	pre := countSegmentDirsOnDisk(t, dir, "ns")
	if pre != 5 {
		t.Fatalf("pre-compact segment dirs on disk = %d, want 5", pre)
	}

	if err := idx.Compact(ctx, "ns"); err != nil {
		t.Fatal(err)
	}

	post := countSegmentDirsOnDisk(t, dir, "ns")
	if post >= pre {
		t.Errorf("compaction did not shrink segment dirs on disk: pre=%d post=%d", pre, post)
	}
	// 3 oldest merged into 1 new -> 5 - 3 + 1 = 3.
	if post != 3 {
		t.Errorf("post-compact = %d, want 3", post)
	}
}

func TestLocalE2E_CrossProcessLockRejects(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()

	ws1, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx1, err := wsindex.New(ws1,
		wsindex.WithAutoCompact(false),
		wsindex.WithLockHeartbeat(time.Second), // long enough to stay live
	)
	if err != nil {
		t.Fatal(err)
	}
	defer idx1.Close()
	if err := idx1.Upsert(ctx, "ns",
		[]retrieval.Doc{{ID: "1", Content: "alpha"}}); err != nil {
		t.Fatal(err)
	}

	// Second Index over the SAME on-disk root: simulates a peer
	// process. ensureNamespace's acquireLock should observe the
	// live lock and refuse.
	ws2, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx2, err := wsindex.New(ws2,
		wsindex.WithAutoCompact(false),
		wsindex.WithLockHeartbeat(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	err = idx2.Upsert(ctx, "ns",
		[]retrieval.Doc{{ID: "2", Content: "beta"}})
	if err == nil {
		t.Fatal("expected ErrLocked, got nil")
	}
	if !errdefs.IsConflict(err) {
		t.Errorf("err category = %v, want Conflict", err)
	}
}

func TestLocalE2E_StaleLockTakenOver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()

	// Hand-write a stale .lock directly on disk: HeartbeatAt is
	// way past the staleness window, so the next acquirer must
	// take it over silently.
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := map[string]any{
		"version":      1,
		"holder":       "ghost-process",
		"pid":          99999,
		"acquired_at":  time.Now().Add(-time.Hour),
		"heartbeat_at": time.Now().Add(-time.Hour),
	}
	raw, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(dir, "ns", ".lock"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	ws, err := sdkworkspace.NewLocalWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := wsindex.New(ws, wsindex.WithAutoCompact(false))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Upsert(ctx, "ns",
		[]retrieval.Doc{{ID: "1", Content: "alpha"}}); err != nil {
		t.Errorf("expected stale takeover, got %v", err)
	}

	// Sanity: lockfile now names a non-ghost holder.
	current, err := os.ReadFile(filepath.Join(dir, "ns", ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(current), "ghost-process") {
		t.Errorf("ghost holder still present after takeover: %s", current)
	}
}

// findFirstFile walks segmentsDir looking for any descendant whose
// basename equals targetName, returning the first match. Tests use
// it to locate "docs.jsonl" without hard-coding the segment ID.
func findFirstFile(t *testing.T, segmentsDir, targetName string) (string, error) {
	t.Helper()
	var found string
	err := filepath.Walk(segmentsDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Base(p) == targetName {
			found = p
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", errors.New("not found: " + targetName)
	}
	return found, nil
}

// countSegmentDirsOnDisk counts segment dirs whose meta.json
// commit marker is present. Mirrors the in-package
// segmentDirs helper but talks directly to the filesystem so
// tests can verify what RemoveAll actually did to the disk.
func countSegmentDirsOnDisk(t *testing.T, root, ns string) int {
	t.Helper()
	segDir := filepath.Join(root, ns, "segments")
	entries, err := os.ReadDir(segDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(segDir, e.Name(), "meta.json")); err == nil {
			n++
		}
	}
	return n
}
