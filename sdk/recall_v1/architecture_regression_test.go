package recall_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall_v1"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/journal"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestScopeGuardAppliesToAuditAndForget(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	m, err := recall.New(idx, recall.WithRequireUserID(), recall.WithJournal(journal.NewMemoryJournal()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	aud := m.(recall.Auditable)
	bad := recall.Scope{RuntimeID: "rt1"}
	if _, err := aud.History(ctx, bad, "id"); !errors.Is(err, recall.ErrMissingUserID) {
		t.Fatalf("History missing-user error = %v; want %v", err, recall.ErrMissingUserID)
	}
	if err := aud.Rollback(ctx, bad, "id", time.Now()); !errors.Is(err, recall.ErrMissingUserID) {
		t.Fatalf("Rollback missing-user error = %v; want %v", err, recall.ErrMissingUserID)
	}
	if err := m.Forget(ctx, bad, "id", "test"); !errors.Is(err, recall.ErrMissingUserID) {
		t.Fatalf("Forget missing-user error = %v; want %v", err, recall.ErrMissingUserID)
	}
}

func TestScopeFromNamespaceRejectsEntitySiblings(t *testing.T) {
	scope := recall.Scope{RuntimeID: "rt1", UserID: "alice"}
	if got, ok := recall.ScopeFromNamespace(recall.EntityNamespaceFor(scope)); ok {
		t.Fatalf("V2 entity namespace decoded as entry scope: %+v", got)
	}
	if got, ok := recall.ScopeFromNamespace("ltm_rt1__u_alice__entities"); ok {
		t.Fatalf("legacy V1 entity namespace decoded as entry scope: %+v", got)
	}
	if got, ok := recall.ScopeFromNamespace("ltm_rt1__u_alice"); !ok || got.RuntimeID != "rt1" || got.UserID != "alice" {
		t.Fatalf("legacy V1 entry namespace decode = %+v ok=%v", got, ok)
	}
}

func TestProjectionReplacementPrunesStaleEntityEdges(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithEntityStore(0),
		recall.WithReconcileInterval(-1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := newScope()
	id := "stable-entry-id"
	if got, err := m.Add(ctx, scope, recall.Entry{ID: id, Content: "Alice note", Entities: []string{"alice"}}); err != nil || got != id {
		t.Fatalf("Add #1 id=%q err=%v", got, err)
	}
	if got, err := m.Add(ctx, scope, recall.Entry{ID: id, Content: "Bob note", Entities: []string{"bob"}}); err != nil || got != id {
		t.Fatalf("Add #2 id=%q err=%v", got, err)
	}

	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	alice, _ := store.Lookup(ctx, scope, []string{"alice"}, 0)
	for _, got := range alice {
		if got == id {
			t.Fatalf("stale alice edge still references %q after replacement; lookup=%v", id, alice)
		}
	}
	bob, _ := store.Lookup(ctx, scope, []string{"bob"}, 0)
	if len(bob) != 1 || bob[0] != id {
		t.Fatalf("bob edge missing after replacement: got %v want [%q]", bob, id)
	}
}

func TestDedupHitRepairsProjectionEagerly(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithEntityStore(0),
		recall.WithReconcileInterval(-1),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := newScope()
	id, err := m.Add(ctx, scope, recall.Entry{Content: "Alice likes tea", Entities: []string{"alice"}})
	if err != nil {
		t.Fatalf("Add #1: %v", err)
	}
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{})
	if err := store.Forget(ctx, scope, id); err != nil {
		t.Fatalf("manual projection prune: %v", err)
	}
	if got, _ := store.Lookup(ctx, scope, []string{"alice"}, 0); len(got) != 0 {
		t.Fatalf("test setup failed, alice edge still present: %v", got)
	}

	gotID, err := m.Add(ctx, scope, recall.Entry{Content: "Alice likes tea", Entities: []string{"alice"}})
	if err != nil {
		t.Fatalf("Add dedup hit: %v", err)
	}
	if gotID != id {
		t.Fatalf("dedup returned id %q; want existing %q", gotID, id)
	}
	got, _ := store.Lookup(ctx, scope, []string{"alice"}, 0)
	if len(got) != 1 || got[0] != id {
		t.Fatalf("dedup hit did not eagerly repair projection: got %v want [%q]", got, id)
	}
}

func TestMemoryJobQueueAttemptStartsOnBusinessStart(t *testing.T) {
	ctx := context.Background()
	q := recall.NewMemoryJobQueue()
	id, err := q.Enqueue(ctx, "ns", recall.JobPayload{Scope: newScope()})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	rec, ok, err := q.Lease(ctx, time.Now())
	if err != nil || !ok || rec.ID != id {
		t.Fatalf("Lease rec=%+v ok=%v err=%v", rec, ok, err)
	}
	if rec.State != recall.JobLeased || rec.Attempts != 0 {
		t.Fatalf("leased rec state/attempts = %s/%d; want leased/0", rec.State, rec.Attempts)
	}
	if err := q.Reschedule(ctx, id, time.Now(), ""); err != nil {
		t.Fatalf("Reschedule leased: %v", err)
	}
	rec, err = q.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != recall.JobPending || rec.Attempts != 0 {
		t.Fatalf("pre-start cancel consumed attempt or changed state wrongly: %+v", rec)
	}

	rec, ok, err = q.Lease(ctx, time.Now())
	if err != nil || !ok {
		t.Fatalf("Lease #2 ok=%v err=%v", ok, err)
	}
	started, err := q.Start(ctx, rec.ID, time.Now())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.State != recall.JobRunning || started.Attempts != 1 {
		t.Fatalf("started rec state/attempts = %s/%d; want running/1", started.State, started.Attempts)
	}
}

func TestAwaitJobUsesWallClockTimeout(t *testing.T) {
	ctx := context.Background()
	frozen := time.Date(2026, 5, 18, 1, 2, 3, 0, time.UTC)
	m, err := recall.New(memidx.New(),
		recall.WithRequireUserID(),
		recall.WithAsyncWorkers(0),
		recall.WithClock(func() time.Time { return frozen }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	id, err := m.SaveAsync(ctx, newScope(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "pending forever"}}},
	})
	if err != nil {
		t.Fatalf("SaveAsync: %v", err)
	}
	start := time.Now()
	_, err = m.(recall.JobController).AwaitJob(ctx, id, 25*time.Millisecond)
	if !errors.Is(err, recall.ErrAwaitTimeout) {
		t.Fatalf("AwaitJob err = %v; want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("AwaitJob ignored wall clock timeout; elapsed=%s", elapsed)
	}
}

func TestSweeperFallbackRestartsAfterMutation(t *testing.T) {
	ctx := context.Background()
	inner := memidx.New()
	idx := &noNativeDeleteIndex{inner: inner}
	now := time.Now()
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithClock(func() time.Time { return now }),
		recall.WithSweeper(time.Hour, 2),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()

	scope := newScope()
	past := now.Add(-time.Hour)
	var ids []string
	for i := 0; i < 5; i++ {
		id, err := m.Add(ctx, scope, recall.Entry{
			Content:   fmt.Sprintf("expired %d", i),
			ExpiresAt: &past,
		})
		if err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	if err := m.(interface {
		SweepNamespace(context.Context, string) error
	}).SweepNamespace(ctx, recall.NamespaceFor(scope)); err != nil {
		t.Fatalf("SweepNamespace: %v", err)
	}
	for _, id := range ids {
		if _, ok, _ := inner.Get(ctx, recall.NamespaceFor(scope), id); ok {
			t.Fatalf("expired id %q survived fallback sweep; ids=%v", id, ids)
		}
	}
}

func TestIndexEntityStoreForgetBuffersBeforeMutation(t *testing.T) {
	ctx := context.Background()
	idx := &recordingIndex{inner: memidx.New()}
	store := recall.NewIndexEntityStore(idx, recall.IndexEntityStoreOptions{LinkedCap: 1000, MaxLinkedCount: -1})
	scope := newScope()

	edges := make(map[string][]string, 501)
	for i := 0; i < 501; i++ {
		edges[fmt.Sprintf("entity-%03d", i)] = []string{"entry-1"}
	}
	if err := store.Link(ctx, scope, edges); err != nil {
		t.Fatalf("Link: %v", err)
	}
	idx.reset()
	if err := store.Forget(ctx, scope, "entry-1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if idx.listCalls < 2 {
		t.Fatalf("test did not exercise pagination; listCalls=%d", idx.listCalls)
	}
	if idx.firstUpsertAt != idx.listCalls {
		t.Fatalf("Forget mutated before completing pagination: firstUpsertAt=%d listCalls=%d", idx.firstUpsertAt, idx.listCalls)
	}
}

type noNativeDeleteIndex struct {
	inner *memidx.Index
}

func (i *noNativeDeleteIndex) Upsert(ctx context.Context, ns string, docs []retrieval.Doc) error {
	return i.inner.Upsert(ctx, ns, docs)
}
func (i *noNativeDeleteIndex) Delete(ctx context.Context, ns string, ids []string) error {
	return i.inner.Delete(ctx, ns, ids)
}
func (i *noNativeDeleteIndex) Search(ctx context.Context, ns string, req retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return i.inner.Search(ctx, ns, req)
}
func (i *noNativeDeleteIndex) List(ctx context.Context, ns string, req retrieval.ListRequest) (*retrieval.ListResponse, error) {
	return i.inner.List(ctx, ns, req)
}
func (i *noNativeDeleteIndex) Capabilities() retrieval.Capabilities {
	c := i.inner.Capabilities()
	c.NativeDeleteByFilter = false
	c.Extensions.DeleteByFilter = false
	return c
}
func (i *noNativeDeleteIndex) Close() error { return i.inner.Close() }

type recordingIndex struct {
	inner         *memidx.Index
	listCalls     int
	firstUpsertAt int
}

func (i *recordingIndex) reset() {
	i.listCalls = 0
	i.firstUpsertAt = 0
}
func (i *recordingIndex) Upsert(ctx context.Context, ns string, docs []retrieval.Doc) error {
	if i.listCalls > 0 && i.firstUpsertAt == 0 {
		i.firstUpsertAt = i.listCalls
	}
	return i.inner.Upsert(ctx, ns, docs)
}
func (i *recordingIndex) Delete(ctx context.Context, ns string, ids []string) error {
	return i.inner.Delete(ctx, ns, ids)
}
func (i *recordingIndex) Search(ctx context.Context, ns string, req retrieval.SearchRequest) (*retrieval.SearchResponse, error) {
	return i.inner.Search(ctx, ns, req)
}
func (i *recordingIndex) List(ctx context.Context, ns string, req retrieval.ListRequest) (*retrieval.ListResponse, error) {
	i.listCalls++
	return i.inner.List(ctx, ns, req)
}
func (i *recordingIndex) Capabilities() retrieval.Capabilities {
	return i.inner.Capabilities()
}
func (i *recordingIndex) Close() error { return i.inner.Close() }
func (i *recordingIndex) Get(ctx context.Context, ns, id string) (retrieval.Doc, bool, error) {
	return i.inner.Get(ctx, ns, id)
}
