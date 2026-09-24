package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("FC_PG_DSN")
	if dsn == "" {
		t.Skip("FC_PG_DSN is not set")
	}
	schema := fmt.Sprintf("memory_test_%d", time.Now().UnixNano())
	store, err := Open(context.Background(), dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Reset(context.Background())
		_ = store.Close()
	})
	return store
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
	if values, err := store.List(ctx, ""); err != nil || len(values) != 3 {
		t.Fatalf("list empty prefix = %d entries, err=%v", len(values), err)
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

// TestPoolKeyNormalizesSchema pins the dedupe identity the memory drivers
// compare against Store.DSNKey: a config that omits "schema" describes the
// same pool as one that names DefaultSchema, so log and kv share one pool
// instead of opening two.
func TestPoolKeyNormalizesSchema(t *testing.T) {
	const dsn = "postgres://user@host:5432/db?sslmode=disable"
	if got, want := PoolKey(dsn, ""), PoolKey(dsn, DefaultSchema); got != want {
		t.Fatalf("PoolKey(omitted) = %q, want %q", got, want)
	}
	if got, want := PoolKey(dsn, "other"), dsn+"\x00other"; got != want {
		t.Fatalf("PoolKey(other) = %q, want %q", got, want)
	}
}
