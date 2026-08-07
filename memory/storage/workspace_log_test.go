package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func event(stream, eventType string, seq uint64) Event {
	return Event{
		Stream:  stream,
		Seq:     seq,
		Type:    eventType,
		Payload: json.RawMessage(`{"n":1}`),
	}
}

func TestWorkspaceLogAppendReadSeqContinuity(t *testing.T) {
	for name, ws := range testWorkspaces(t) {
		t.Run(name, func(t *testing.T) {
			store, err := NewWorkspaceLog(ws)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			first, err := store.Append(ctx, "rt/u/a/conv", []Event{
				event("rt/u/a/conv", "one", 0),
				event("rt/u/a/conv", "two", 0),
			}, AppendOptions{IdempotencyKey: "k1"})
			if err != nil {
				t.Fatal(err)
			}
			if first.FirstSeq != 1 || first.LastSeq != 2 {
				t.Fatalf("first commit seq = %d..%d, want 1..2", first.FirstSeq, first.LastSeq)
			}
			second, err := store.Append(ctx, "rt/u/a/conv", []Event{
				event("rt/u/a/conv", "three", 0),
			}, AppendOptions{IdempotencyKey: "k2"})
			if err != nil {
				t.Fatal(err)
			}
			if second.FirstSeq != 3 || second.LastSeq != 3 {
				t.Fatalf("second commit seq = %d..%d, want 3..3", second.FirstSeq, second.LastSeq)
			}
			got, err := store.Read(ctx, "rt/u/a/conv", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			wantSeq := []uint64{1, 2, 3}
			for index, value := range got {
				if value.Seq != wantSeq[index] {
					t.Fatalf("event %d seq = %d, want %d", index, value.Seq, wantSeq[index])
				}
				if value.Type != []string{"one", "two", "three"}[index] {
					t.Fatalf("event %d type = %q", index, value.Type)
				}
			}
			latest, err := store.ReadLatest(ctx, "rt/u/a/conv", 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(latest) != 2 || latest[0].Seq != 2 || latest[1].Seq != 3 {
				t.Fatalf("latest = %+v", latest)
			}
			all, err := store.ReadLatest(ctx, "rt/u/a/conv", 0)
			if err != nil || len(all) != 3 {
				t.Fatalf("latest all = %+v, %v", all, err)
			}
		})
	}
}

func TestWorkspaceLogIdempotentRetryAndConflict(t *testing.T) {
	store, err := NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stream := "rt/u/a/conv"
	batch := []Event{event(stream, "same", 0)}
	first, err := store.Append(ctx, stream, batch, AppendOptions{IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.Append(ctx, stream, batch, AppendOptions{IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != first.ID || retry.FirstSeq != first.FirstSeq || retry.LastSeq != first.LastSeq {
		t.Fatalf("retry = %+v, want %+v", retry, first)
	}
	got, err := store.Read(ctx, stream, 0, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("read after retry = %+v, %v", got, err)
	}
	if _, err := store.Append(ctx, stream, []Event{event(stream, "different", 0)},
		AppendOptions{IdempotencyKey: "key"}); !errors.Is(err, ErrConflict) || !errdefs.IsConflict(err) {
		t.Fatalf("different batch under same key error = %v, want ErrConflict", err)
	}
}

func TestWorkspaceLogReadPaginationAndMissing(t *testing.T) {
	store, err := NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stream := "rt/u/a/conv"
	for index := 1; index <= 5; index++ {
		if _, err := store.Append(ctx, stream, []Event{event(stream, fmt.Sprintf("e%d", index), 0)},
			AppendOptions{IdempotencyKey: fmt.Sprintf("k%d", index)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Read(ctx, stream, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].Seq != 2 || page[1].Seq != 3 {
		t.Fatalf("page = %+v", page)
	}
	empty, err := store.Read(ctx, stream, 5, 0)
	if err != nil || len(empty) != 0 {
		t.Fatalf("after-head read = %+v, %v", empty, err)
	}
	if _, err := store.ReadAt(ctx, stream, 99); !errors.Is(err, ErrNotFound) || !errdefs.IsNotFound(err) {
		t.Fatalf("ReadAt missing error = %v, want ErrNotFound", err)
	}
	missing, err := store.Read(ctx, "rt/u/a/other", 0, 0)
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing stream read = %+v, %v", missing, err)
	}
}

func TestWorkspaceLogListStreamsPrefix(t *testing.T) {
	store, err := NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	streams := []string{
		"rt/u/a/conv1",
		"rt/u/a/conv2",
		"rt/u/ab/conv4",
		"rt/u/b/conv3",
	}
	for index, stream := range streams {
		if _, err := store.Append(ctx, stream, []Event{event(stream, "x", 0)},
			AppendOptions{IdempotencyKey: fmt.Sprintf("k%d", index)}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.ListStreams(ctx, "")
	if err != nil || !reflect.DeepEqual(all, streams) {
		t.Fatalf("ListStreams(\"\") = %v, %v; want %v", all, err, streams)
	}
	a, err := store.ListStreams(ctx, "rt/u/a")
	if err != nil || !reflect.DeepEqual(a, []string{"rt/u/a/conv1", "rt/u/a/conv2"}) {
		t.Fatalf("ListStreams(rt/u/a) = %v, %v", a, err)
	}
	// Segment-boundary matching must not leak into the sibling partition
	// "rt/u/ab/...".
	for _, stream := range a {
		if stream == "rt/u/ab/conv4" {
			t.Fatalf("ListStreams(rt/u/a) leaked sibling stream %q", stream)
		}
	}
	exact, err := store.ListStreams(ctx, "rt/u/a/conv1")
	if err != nil || !reflect.DeepEqual(exact, []string{"rt/u/a/conv1"}) {
		t.Fatalf("ListStreams(exact) = %v, %v", exact, err)
	}
	none, err := store.ListStreams(ctx, "rt/x")
	if err != nil || len(none) != 0 {
		t.Fatalf("ListStreams(missing) = %v, %v", none, err)
	}
}

func TestWorkspaceLogCrashRecoveryRollback(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store, err := NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stream := "rt/u/a/conv"
	dir, err := store.streamDir(stream)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: pending marker plus one partial event, no commit
	// marker, no head.
	partial := event(stream, "partial", 0)
	partial.Seq = 1
	if err := writeJSON(ctx, ws, store.eventPath(dir, 1), partial); err != nil {
		t.Fatal(err)
	}
	pending := persistedPending{
		SchemaVersion:  logSchemaVersion,
		IdempotencyKey: "crashed",
		FirstSeq:       1,
		LastSeq:        2,
		Digest:         "digest",
		CreatedAt:      partial.CreatedAt,
	}
	if err := writeJSON(ctx, ws, store.pendingPath(dir), pending); err != nil {
		t.Fatal(err)
	}
	// A fresh adapter (and any operation) must roll the partial batch back.
	fresh, err := NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := fresh.Append(ctx, stream, []Event{event(stream, "new", 0)},
		AppendOptions{IdempotencyKey: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if commit.FirstSeq != 1 {
		t.Fatalf("first seq after rollback = %d, want 1", commit.FirstSeq)
	}
	got, err := fresh.Read(ctx, stream, 0, 0)
	if err != nil || len(got) != 1 || got[0].Type != "new" {
		t.Fatalf("read after rollback = %+v, %v", got, err)
	}
	if exists, err := ws.Exists(ctx, store.pendingPath(dir)); err != nil || exists {
		t.Fatalf("pending after recovery = %v, %v", exists, err)
	}
}

func TestWorkspaceLogCrashRecoveryPublishForward(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	store, err := NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stream := "rt/u/a/conv"
	dir, err := store.streamDir(stream)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after events and the commit marker were written but
	// before the head was published.
	committed := event(stream, "committed", 0)
	committed.Seq = 1
	digest, err := batchDigest(stream, "key", []Event{event(stream, "committed", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(ctx, ws, store.eventPath(dir, 1), committed); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(ctx, ws, store.commitPath(dir, "key"), persistedCommit{
		SchemaVersion:  logSchemaVersion,
		IdempotencyKey: "key",
		FirstSeq:       1,
		LastSeq:        1,
		Digest:         digest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(ctx, ws, store.pendingPath(dir), persistedPending{
		SchemaVersion:  logSchemaVersion,
		IdempotencyKey: "key",
		FirstSeq:       1,
		LastSeq:        1,
		Digest:         digest,
	}); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fresh.Read(ctx, stream, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "committed" {
		t.Fatalf("read after publish-forward = %+v", got)
	}
	if exists, err := ws.Exists(ctx, store.pendingPath(dir)); err != nil || exists {
		t.Fatalf("pending after recovery = %v, %v", exists, err)
	}
}

func TestWorkspaceLogListStreamsPublishForwardAfterCrash(t *testing.T) {
	for name, ws := range testWorkspaces(t) {
		t.Run(name, func(t *testing.T) {
			store, err := NewWorkspaceLog(ws)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			stream := "rt/u/a/conv"
			dir, err := store.streamDir(stream)
			if err != nil {
				t.Fatal(err)
			}
			committed := event(stream, "committed", 0)
			committed.Seq = 1
			digest, err := batchDigest(stream, "key", []Event{event(stream, "committed", 0)})
			if err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(ctx, ws, store.eventPath(dir, 1), committed); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(ctx, ws, store.commitPath(dir, "key"), persistedCommit{
				SchemaVersion:  logSchemaVersion,
				IdempotencyKey: "key",
				FirstSeq:       1,
				LastSeq:        1,
				Digest:         digest,
			}); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(ctx, ws, store.pendingPath(dir), persistedPending{
				SchemaVersion:  logSchemaVersion,
				IdempotencyKey: "key",
				FirstSeq:       1,
				LastSeq:        1,
				Digest:         digest,
			}); err != nil {
				t.Fatal(err)
			}

			fresh, err := NewWorkspaceLog(ws)
			if err != nil {
				t.Fatal(err)
			}
			streams, err := fresh.ListStreams(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(streams, []string{stream}) {
				t.Fatalf("ListStreams after publish-forward = %v, want %v", streams, []string{stream})
			}
			if exists, err := ws.Exists(ctx, store.headPath(dir)); err != nil || !exists {
				t.Fatalf("head after ListStreams = %v, %v", exists, err)
			}
			if exists, err := ws.Exists(ctx, store.pendingPath(dir)); err != nil || exists {
				t.Fatalf("pending after ListStreams = %v, %v", exists, err)
			}
			commits, err := fresh.ListCommits(ctx, stream, 0, 0)
			if err != nil || len(commits) != 1 || commits[0].IdempotencyKey != "key" {
				t.Fatalf("ListCommits after ListStreams = %#v, %v", commits, err)
			}
		})
	}
}

func TestWorkspaceLogListStreamsRollsBackPendingAfterCrash(t *testing.T) {
	for name, ws := range testWorkspaces(t) {
		t.Run(name, func(t *testing.T) {
			store, err := NewWorkspaceLog(ws)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			stream := "rt/u/a/conv"
			dir, err := store.streamDir(stream)
			if err != nil {
				t.Fatal(err)
			}
			partial := event(stream, "partial", 0)
			partial.Seq = 1
			if err := writeJSON(ctx, ws, store.eventPath(dir, 1), partial); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(ctx, ws, store.pendingPath(dir), persistedPending{
				SchemaVersion:  logSchemaVersion,
				IdempotencyKey: "crashed",
				FirstSeq:       1,
				LastSeq:        2,
				Digest:         "digest",
			}); err != nil {
				t.Fatal(err)
			}

			fresh, err := NewWorkspaceLog(ws)
			if err != nil {
				t.Fatal(err)
			}
			streams, err := fresh.ListStreams(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(streams) != 0 {
				t.Fatalf("ListStreams after rollback = %v, want empty", streams)
			}
			if exists, err := ws.Exists(ctx, store.eventPath(dir, 1)); err != nil || exists {
				t.Fatalf("partial event after rollback = %v, %v", exists, err)
			}
			commit, err := fresh.Append(ctx, stream, []Event{event(stream, "new", 0)},
				AppendOptions{IdempotencyKey: "new"})
			if err != nil {
				t.Fatal(err)
			}
			if commit.FirstSeq != 1 {
				t.Fatalf("first seq after rollback = %d, want 1", commit.FirstSeq)
			}
		})
	}
}

func TestWorkspaceLogCommitLogListAndReadByKey(t *testing.T) {
	for name, ws := range testWorkspaces(t) {
		t.Run(name, func(t *testing.T) {
			store, err := NewWorkspaceLog(ws)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			stream := "rt/u/a/conv"
			first, err := store.Append(ctx, stream, []Event{
				event(stream, "one", 0),
				event(stream, "two", 0),
			}, AppendOptions{IdempotencyKey: "k1"})
			if err != nil {
				t.Fatal(err)
			}
			second, err := store.Append(ctx, stream, []Event{event(stream, "three", 0)},
				AppendOptions{IdempotencyKey: "k2"})
			if err != nil {
				t.Fatal(err)
			}

			all, err := store.ListCommits(ctx, stream, 0, 0)
			if err != nil || len(all) != 2 {
				t.Fatalf("ListCommits all = %#v, %v", all, err)
			}
			if all[0].ID != first.ID || all[0].FirstSeq != 1 || all[0].LastSeq != 2 ||
				all[1].ID != second.ID || all[1].FirstSeq != 3 || all[1].LastSeq != 3 {
				t.Fatalf("ListCommits order = %#v", all)
			}
			after, err := store.ListCommits(ctx, stream, first.LastSeq, 1)
			if err != nil || len(after) != 1 || after[0].ID != second.ID {
				t.Fatalf("ListCommits after = %#v, %v", after, err)
			}
			byKey, found, err := store.ReadCommitByKey(ctx, stream, "k1")
			if err != nil || !found || byKey.FirstSeq != 1 || byKey.LastSeq != 2 {
				t.Fatalf("ReadCommitByKey = %#v, %v, %v", byKey, found, err)
			}
			if _, found, err := store.ReadCommitByKey(ctx, stream, "missing"); err != nil || found {
				t.Fatalf("missing commit = %v, %v", found, err)
			}
			empty, err := store.ListCommits(ctx, "rt/u/a/other", 0, 0)
			if err != nil || len(empty) != 0 {
				t.Fatalf("ListCommits missing stream = %#v, %v", empty, err)
			}

			reopened, err := NewWorkspaceLog(ws)
			if err != nil {
				t.Fatal(err)
			}
			allAgain, err := reopened.ListCommits(ctx, stream, 0, 0)
			if err != nil || len(allAgain) != 2 || allAgain[0].ID != first.ID {
				t.Fatalf("ListCommits after reopen = %#v, %v", allAgain, err)
			}
		})
	}
}

func TestWorkspaceLogAppendValidates(t *testing.T) {
	store, err := NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Append(ctx, "rt/u/a/conv", []Event{
		event("rt/u/a/OTHER", "x", 0),
	}, AppendOptions{IdempotencyKey: "k"}); err == nil {
		t.Fatal("Append accepted a stream mismatch")
	}
	if _, err := store.Append(ctx, "rt/u/a/conv", nil, AppendOptions{IdempotencyKey: "k"}); err == nil {
		t.Fatal("Append accepted no events")
	}
	if _, err := store.Append(ctx, "rt/u/a/conv", []Event{event("rt/u/a/conv", "x", 0)},
		AppendOptions{IdempotencyKey: ""}); err == nil {
		t.Fatal("Append accepted an empty idempotency key")
	}
	if _, err := store.Append(ctx, "rt/../escape", []Event{event("rt/../escape", "x", 0)},
		AppendOptions{IdempotencyKey: "k"}); err == nil {
		t.Fatal("Append accepted an unsafe stream")
	}
}

func testWorkspaces(t *testing.T) map[string]workspace.Workspace {
	t.Helper()
	workspaces := map[string]workspace.Workspace{
		"mem": workspace.NewMemWorkspace(),
	}
	local, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaces["local"] = local
	return workspaces
}
