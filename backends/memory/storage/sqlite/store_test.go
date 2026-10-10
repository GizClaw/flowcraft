package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestListEmptyPrefixReturnsEverything pins the cross-driver contract: an
// empty prefix lists every key, matching the workspace driver.
func TestListEmptyPrefixReturnsEverything(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, key := range []string{"views/a", "views/b", "sources/c"} {
		if err := store.Put(ctx, key, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("List(\"\") = %d entries, want 3", len(entries))
	}
	if entries[0].Key != "sources/c" || entries[2].Key != "views/b" {
		t.Fatalf("List(\"\") order = %#v", entries)
	}
}

// TestPooledConnectionsCarryPragmas guards the per-connection pragmas: with
// a busy_timeout of zero a second connection fails immediately on a locked
// database instead of waiting. The file-backed DSN must therefore apply the
// pragma to every connection, not only the one that ran migrate.
func TestPooledConnectionsCarryPragmas(t *testing.T) {
	path := t.TempDir() + "/store.db"
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	// Force two distinct connections by holding the first one open.
	held, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if _, err := held.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = held.ExecContext(context.Background(), "ROLLBACK") }()
	other, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	var timeout int
	if err := other.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 5000 {
		t.Fatalf("pooled connection busy_timeout = %d, want 5000", timeout)
	}
}

func TestLogAppendReadIdempotencyAndConflict(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	stream := "rt/u/a/conv"
	events := []storage.Event{{Stream: stream, Type: "record", Payload: []byte(`{"n":1}`)}}
	first, err := store.Append(ctx, stream, events, storage.AppendOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if first.FirstSeq != 1 || first.LastSeq != 1 {
		t.Fatalf("first commit = %#v", first)
	}
	replay, err := store.Append(ctx, stream, events, storage.AppendOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID || replay.FirstSeq != 1 {
		t.Fatalf("replay = %#v, want the original commit", replay)
	}
	conflict := []storage.Event{{Stream: stream, Type: "record", Payload: []byte(`{"n":2}`)}}
	if _, err := store.Append(ctx, stream, conflict, storage.AppendOptions{IdempotencyKey: "k1"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	second, err := store.Append(ctx, stream, []storage.Event{{Stream: stream, Type: "record", Payload: []byte(`{"n":2}`)}},
		storage.AppendOptions{IdempotencyKey: "k2"})
	if err != nil {
		t.Fatal(err)
	}
	if second.FirstSeq != 2 || second.LastSeq != 2 {
		t.Fatalf("second commit = %#v", second)
	}
	read, err := store.Read(ctx, stream, 0, 0)
	if err != nil || len(read) != 2 {
		t.Fatalf("read = %d events, err=%v", len(read), err)
	}
	at, err := store.ReadAt(ctx, stream, 2)
	if err != nil || string(at.Payload) != `{"n":2}` {
		t.Fatalf("read at = %#v, err=%v", at, err)
	}
	if _, err := store.ReadAt(ctx, stream, 99); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing read at error = %v", err)
	}
	latest, err := store.ReadLatest(ctx, stream, 1)
	if err != nil || len(latest) != 1 || latest[0].Seq != 2 {
		t.Fatalf("latest = %#v, err=%v", latest, err)
	}
	streams, err := store.ListStreams(ctx, "rt/u")
	if err != nil || len(streams) != 1 || streams[0] != stream {
		t.Fatalf("streams = %#v, err=%v", streams, err)
	}
	if _, found, err := store.ReadCommitByKey(ctx, stream, "k1"); err != nil || !found {
		t.Fatalf("read commit by key found=%v err=%v", found, err)
	}
	commits, err := store.ListCommits(ctx, stream, 0, 0)
	if err != nil || len(commits) != 2 || commits[1].FirstSeq != 2 {
		t.Fatalf("commits = %#v, err=%v", commits, err)
	}
}

func TestKVContract(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.Get(ctx, "facts/missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing get error = %v", err)
	}
	if err := store.Put(ctx, "facts/a", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "facts/ab", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Get(ctx, "facts/a"); err != nil || string(value) != "one" {
		t.Fatalf("get = %q, err=%v", value, err)
	}
	entries, err := store.List(ctx, "facts/a")
	if err != nil || len(entries) != 1 || entries[0].Key != "facts/a" {
		t.Fatalf("list boundary = %#v, err=%v", entries, err)
	}
	if err := store.Delete(ctx, "facts/missing"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if written, err := store.PutIfAbsent(ctx, "facts/a", []byte("other")); err != nil || written {
		t.Fatalf("put if absent existing written=%v err=%v", written, err)
	}
	if written, err := store.PutIfAbsent(ctx, "facts/b", []byte("new")); err != nil || !written {
		t.Fatalf("put if absent missing written=%v err=%v", written, err)
	}
	if swapped, err := store.CompareAndSwap(ctx, "facts/a", []byte("wrong"), []byte("x")); err != nil || swapped {
		t.Fatalf("cas wrong swapped=%v err=%v", swapped, err)
	}
	if swapped, err := store.CompareAndSwap(ctx, "facts/a", []byte("one"), []byte("x")); err != nil || !swapped {
		t.Fatalf("cas right swapped=%v err=%v", swapped, err)
	}
	if values, err := store.List(ctx, "facts"); err != nil || len(values) != 3 {
		t.Fatalf("list all = %d entries, err=%v", len(values), err)
	}
	if err := store.PutBatch(ctx, []storage.Entry{
		{Key: "batch/a", Value: []byte("1")},
		{Key: "batch/b", Value: []byte("2")},
	}); err != nil {
		t.Fatal(err)
	}
	if values, err := store.List(ctx, "batch"); err != nil || len(values) != 2 {
		t.Fatalf("batch list = %d entries, err=%v", len(values), err)
	}
}
