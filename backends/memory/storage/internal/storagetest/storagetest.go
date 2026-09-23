// Package storagetest provides the shared conformance suite for the storage
// drivers, so workspace, SQLite, and PostgreSQL backends are asserted against
// the same Log/KV contracts instead of hand-duplicated variants.
package storagetest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
)

// Backend is one driver instance under test. Log and KV must be empty when
// Run is called.
type Backend struct {
	Log storage.Log
	KV  storage.Store
}

// Run executes the conformance suite.
func Run(t *testing.T, backend Backend) {
	t.Helper()
	if backend.Log == nil || backend.KV == nil {
		t.Fatal("storagetest: log and kv are required")
	}
	t.Run("LogAppendReadAndIdempotency", func(t *testing.T) { testLogAppendRead(t, backend.Log) })
	t.Run("LogCommitsAndStreams", func(t *testing.T) { testLogCommitsAndStreams(t, backend.Log) })
	t.Run("KVValues", func(t *testing.T) { testKVValues(t, backend.KV) })
}

func testLogAppendRead(t *testing.T, log storage.Log) {
	t.Helper()
	ctx := context.Background()
	const stream = "conformance/append/basic"
	batch := []storage.Event{
		{Stream: stream, Type: "message", Payload: []byte(`{"n":1}`)},
		{Stream: stream, Type: "message", Payload: []byte(`{"n":2}`)},
	}
	commit, err := log.Append(ctx, stream, batch, storage.AppendOptions{IdempotencyKey: "turn-1"})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if commit.Stream != stream || commit.FirstSeq != 1 || commit.LastSeq != 2 || commit.ID == "" {
		t.Fatalf("commit = %#v", commit)
	}
	replayed, err := log.Append(ctx, stream, batch, storage.AppendOptions{IdempotencyKey: "turn-1"})
	if err != nil {
		t.Fatalf("idempotent append: %v", err)
	}
	if replayed.ID != commit.ID || replayed.FirstSeq != commit.FirstSeq || replayed.LastSeq != commit.LastSeq {
		t.Fatalf("replayed commit = %#v, want %#v", replayed, commit)
	}
	conflict := []storage.Event{{Stream: stream, Type: "message", Payload: []byte(`{"n":3}`)}}
	if _, err := log.Append(ctx, stream, conflict, storage.AppendOptions{IdempotencyKey: "turn-1"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("conflicting append err = %v, want ErrConflict", err)
	}
	events, err := log.Read(ctx, stream, 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("events = %#v", events)
	}
	if !bytes.Equal(events[1].Payload, []byte(`{"n":2}`)) || events[1].Type != "message" {
		t.Fatalf("event payload/type = %s/%s", events[1].Payload, events[1].Type)
	}
	tail, err := log.Read(ctx, stream, 1, 0)
	if err != nil || len(tail) != 1 || tail[0].Seq != 2 {
		t.Fatalf("read after = %#v, %v", tail, err)
	}
	latest, err := log.ReadLatest(ctx, stream, 1)
	if err != nil || len(latest) != 1 || latest[0].Seq != 2 {
		t.Fatalf("read latest = %#v, %v", latest, err)
	}
	if _, err := log.ReadAt(ctx, stream, 2); err != nil {
		t.Fatalf("read at: %v", err)
	}
	if _, err := log.ReadAt(ctx, stream, 99); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("read missing err = %v, want ErrNotFound", err)
	}
	next, err := log.Append(ctx, stream, conflict, storage.AppendOptions{IdempotencyKey: "turn-2"})
	if err != nil || next.FirstSeq != 3 || next.LastSeq != 3 {
		t.Fatalf("second append = %#v, %v", next, err)
	}
}

func testLogCommitsAndStreams(t *testing.T, log storage.Log) {
	t.Helper()
	ctx := context.Background()
	for _, stream := range []string{"conformance/streams/alpha/a", "conformance/streams/alpha/b", "conformance/streams/beta/c"} {
		if _, err := log.Append(ctx, stream, []storage.Event{{Stream: stream, Type: "message"}},
			storage.AppendOptions{IdempotencyKey: "key-" + stream}); err != nil {
			t.Fatalf("append %s: %v", stream, err)
		}
	}
	streams, err := log.ListStreams(ctx, "conformance/streams/alpha")
	if err != nil {
		t.Fatalf("list streams: %v", err)
	}
	want := []string{"conformance/streams/alpha/a", "conformance/streams/alpha/b"}
	if len(streams) != len(want) || streams[0] != want[0] || streams[1] != want[1] {
		t.Fatalf("streams = %v, want %v", streams, want)
	}
	all, err := log.ListStreams(ctx, "conformance/streams")
	if err != nil || len(all) != 3 {
		t.Fatalf("all streams = %v, %v", all, err)
	}
	commitLog, ok := log.(storage.CommitLog)
	if !ok {
		return
	}
	commit, found, err := commitLog.ReadCommitByKey(ctx, "conformance/streams/alpha/a", "key-conformance/streams/alpha/a")
	if err != nil || !found || commit.FirstSeq != 1 {
		t.Fatalf("read commit = %#v/%v/%v", commit, found, err)
	}
	if _, found, err := commitLog.ReadCommitByKey(ctx, "conformance/streams/alpha/a", "missing"); err != nil || found {
		t.Fatalf("missing commit = %v/%v, want false/nil", found, err)
	}
}

func testKVValues(t *testing.T, kv storage.Store) {
	t.Helper()
	ctx := context.Background()
	const key = "conformance/values/value"
	if _, err := kv.Get(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing get err = %v, want ErrNotFound", err)
	}
	if err := kv.Put(ctx, key, []byte("one")); err != nil {
		t.Fatalf("put: %v", err)
	}
	value, err := kv.Get(ctx, key)
	if err != nil || string(value) != "one" {
		t.Fatalf("get = %q, %v", value, err)
	}
	if err := kv.Put(ctx, "conformance/values/other", []byte("two")); err != nil {
		t.Fatalf("put other: %v", err)
	}
	entries, err := kv.List(ctx, "conformance/values")
	if err != nil || len(entries) != 2 ||
		entries[0].Key != "conformance/values/other" || entries[1].Key != key {
		t.Fatalf("list = %#v, %v", entries, err)
	}
	if err := kv.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := kv.Delete(ctx, key); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if _, err := kv.Get(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if cas, ok := kv.(storage.CASStore); ok {
		if err := kv.Put(ctx, key, []byte("a")); err != nil {
			t.Fatalf("cas put: %v", err)
		}
		swapped, err := cas.CompareAndSwap(ctx, key, []byte("b"), []byte("c"))
		if err != nil || swapped {
			t.Fatalf("mismatched cas = %v/%v, want false/nil", swapped, err)
		}
		swapped, err = cas.CompareAndSwap(ctx, key, []byte("a"), []byte("b"))
		if err != nil || !swapped {
			t.Fatalf("matching cas = %v/%v, want true/nil", swapped, err)
		}
		if value, err := kv.Get(ctx, key); err != nil || string(value) != "b" {
			t.Fatalf("value after cas = %q, %v", value, err)
		}
	}
	if putIfAbsent, ok := kv.(storage.PutIfAbsentStore); ok {
		created, err := putIfAbsent.PutIfAbsent(ctx, "conformance/values/immutable", []byte("first"))
		if err != nil || !created {
			t.Fatalf("put if absent = %v/%v, want true/nil", created, err)
		}
		created, err = putIfAbsent.PutIfAbsent(ctx, "conformance/values/immutable", []byte("second"))
		if err != nil || created {
			t.Fatalf("second put if absent = %v/%v, want false/nil", created, err)
		}
		if value, err := kv.Get(ctx, "conformance/values/immutable"); err != nil || string(value) != "first" {
			t.Fatalf("immutable value = %q, %v", value, err)
		}
	}
	if batch, ok := kv.(storage.BatchStore); ok {
		if err := batch.PutBatch(ctx, []storage.Entry{
			{Key: "conformance/values/batch/a", Value: []byte("a")},
			{Key: "conformance/values/batch/b", Value: []byte("b")},
		}); err != nil {
			t.Fatalf("put batch: %v", err)
		}
		if value, err := kv.Get(ctx, "conformance/values/batch/b"); err != nil || string(value) != "b" {
			t.Fatalf("batched value = %q, %v", value, err)
		}
	}
}
