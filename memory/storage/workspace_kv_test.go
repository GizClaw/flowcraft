package storage

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestWorkspaceKVPutGetDelete(t *testing.T) {
	store, err := NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, "facts/a", []byte("one")); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "facts/a")
	if err != nil || string(got) != "one" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := store.Put(ctx, "facts/a", []byte("two")); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, "facts/a")
	if err != nil || string(got) != "two" {
		t.Fatalf("Get after overwrite = %q, %v", got, err)
	}
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) || !errdefs.IsNotFound(err) {
		t.Fatalf("Get missing error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "facts/a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "facts/a"); err != nil {
		t.Fatalf("Delete missing error = %v, want nil", err)
	}
	if _, err := store.Get(ctx, "facts/a"); !errors.Is(err, ErrNotFound) || !errdefs.IsNotFound(err) {
		t.Fatalf("Get after delete error = %v, want ErrNotFound", err)
	}
}

func TestWorkspaceKVListPrefixScan(t *testing.T) {
	store, err := NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	entries := map[string]string{
		"facts/a":   "fa",
		"facts/ab":  "fabs",
		"facts/b":   "fb",
		"facts/c/1": "fc1",
		"notes/x":   "nx",
	}
	for key, value := range entries {
		if err := store.Put(ctx, key, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(entries) {
		t.Fatalf("List(\"\") = %d entries, want %d", len(all), len(entries))
	}
	facts, err := store.List(ctx, "facts")
	if err != nil {
		t.Fatal(err)
	}
	wantFacts := []string{"facts/a", "facts/ab", "facts/b", "facts/c/1"}
	if !reflect.DeepEqual(keysOf(facts), wantFacts) {
		t.Fatalf("List(facts) keys = %v, want %v", keysOf(facts), wantFacts)
	}
	prefixA, err := store.List(ctx, "facts/a")
	if err != nil {
		t.Fatal(err)
	}
	// Segment-boundary matching: "facts/a" must not match the sibling key
	// "facts/ab".
	if !reflect.DeepEqual(keysOf(prefixA), []string{"facts/a"}) {
		t.Fatalf("List(facts/a) keys = %v", keysOf(prefixA))
	}
	nested, err := store.List(ctx, "facts/c")
	if err != nil || !reflect.DeepEqual(keysOf(nested), []string{"facts/c/1"}) {
		t.Fatalf("List(facts/c) = %v, %v", keysOf(nested), err)
	}
	exact, err := store.List(ctx, "facts/ab")
	if err != nil || !reflect.DeepEqual(keysOf(exact), []string{"facts/ab"}) {
		t.Fatalf("List(facts/ab) = %v, %v", keysOf(exact), err)
	}
	missing, err := store.List(ctx, "nothing")
	if err != nil || len(missing) != 0 {
		t.Fatalf("List(missing) = %+v, %v", missing, err)
	}
}

func TestWorkspaceKVPutIfAbsentAndCAS(t *testing.T) {
	store, err := NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ok, err := store.PutIfAbsent(ctx, "catalog/entry", []byte("v1"))
	if err != nil || !ok {
		t.Fatalf("first PutIfAbsent = %v, %v", ok, err)
	}
	ok, err = store.PutIfAbsent(ctx, "catalog/entry", []byte("v2"))
	if err != nil || ok {
		t.Fatalf("second PutIfAbsent = %v, %v", ok, err)
	}
	got, _ := store.Get(ctx, "catalog/entry")
	if string(got) != "v1" {
		t.Fatalf("PutIfAbsent changed value to %q", got)
	}
	swapped, err := store.CompareAndSwap(ctx, "catalog/entry", []byte("v1"), []byte("v3"))
	if err != nil || !swapped {
		t.Fatalf("CAS match = %v, %v", swapped, err)
	}
	swapped, err = store.CompareAndSwap(ctx, "catalog/entry", []byte("wrong"), []byte("v4"))
	if err != nil || swapped {
		t.Fatalf("CAS mismatch = %v, %v", swapped, err)
	}
	swapped, err = store.CompareAndSwap(ctx, "missing", []byte("old"), []byte("new"))
	if err != nil || swapped {
		t.Fatalf("CAS missing = %v, %v", swapped, err)
	}
	got, _ = store.Get(ctx, "catalog/entry")
	if string(got) != "v3" {
		t.Fatalf("value after CAS = %q", got)
	}
}

func TestWorkspaceKVInvalidKeysAndPrefixConflict(t *testing.T) {
	store, err := NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"..", "a//b", "/a", "a/", "a\x00b", "."} {
		if err := store.Put(ctx, key, []byte("x")); err == nil {
			t.Fatalf("Put(%q) succeeded", key)
		}
	}
	if err := store.Put(ctx, "a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "a/b", []byte("y")); err == nil {
		t.Fatal("Put under an existing file key succeeded")
	}
}

func TestWorkspaceKVValuesRoundTrip(t *testing.T) {
	store, err := NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	empty := []byte{}
	if err := store.Put(ctx, "k", empty); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(ctx, "k")
	if err != nil || len(list) != 1 || !bytes.Equal(list[0].Value, empty) {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func keysOf(entries []Entry) []string {
	keys := make([]string, len(entries))
	for index, entry := range entries {
		keys[index] = entry.Key
	}
	return keys
}
