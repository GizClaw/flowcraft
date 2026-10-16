package fact

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
)

// pageStore counts the fact payloads a read pays for, and pageLog the merge
// streams behind them: together they are what one List costs.
type pageStore struct {
	storage.Store
	storage.PutIfAbsentStore

	gets int
}

func (store *pageStore) Get(ctx context.Context, key string) ([]byte, error) {
	store.gets++
	return store.Store.Get(ctx, key)
}

type pageLog struct {
	storage.Log
	storage.CommitLog

	reads int
}

func (log *pageLog) Read(ctx context.Context, stream string, after uint64, limit int) ([]storage.Event, error) {
	log.reads++
	return log.Log.Read(ctx, stream, after, limit)
}

type pageFixture struct {
	store *FactStore
	kv    *pageStore
	log   *pageLog
}

func newPageFixture(t *testing.T, facts int) *pageFixture {
	t.Helper()
	ws := newTestWorkspace(t)
	rawLog, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	rawKV, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	logStore := &pageLog{Log: rawLog, CommitLog: rawLog}
	kvStore := &pageStore{Store: rawKV, PutIfAbsentStore: rawKV}
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store, err := NewFactStore(logStore, kvStore, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for index := 0; index < facts; index++ {
		if _, err := store.Add(ctx, factRequest(fmt.Sprintf("fact-%02d", index), fmt.Sprintf("content %d", index))); err != nil {
			t.Fatal(err)
		}
	}
	fixture := &pageFixture{store: store, kv: kvStore, log: logStore}
	fixture.reset()
	return fixture
}

func (fixture *pageFixture) reset() { fixture.kv.gets, fixture.log.reads = 0, 0 }

func (fixture *pageFixture) list(t *testing.T, options ListOptions) []Fact {
	t.Helper()
	values, err := fixture.store.List(context.Background(), factScope, "conversation", options)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func (fixture *pageFixture) ids(values []Fact) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.ID)
	}
	return result
}

// TestListPageReadsTheFactsOnIt is the bound the page exists for: reading the
// conversation whole costs every fact, and a page costs the facts it returns --
// a caller paging through pays for each fact once, not for the window behind
// every page.
func TestListPageReadsTheFactsOnIt(t *testing.T) {
	const facts = 40
	// Every List resolves the conversation's generation first: one payload read
	// that is not a fact.
	const generationPointer = 1
	fixture := newPageFixture(t, facts)
	whole := fixture.list(t, ListOptions{})
	if len(whole) != facts {
		t.Fatalf("whole list = %d facts", len(whole))
	}
	t.Logf("whole list of %d facts: %d payload reads, %d merge-stream reads", facts, fixture.kv.gets, fixture.log.reads)
	if fixture.kv.gets != facts+generationPointer || fixture.log.reads != facts {
		t.Fatalf("whole list read %d payloads and %d merge streams", fixture.kv.gets, fixture.log.reads)
	}
	// A page from the start of the conversation.
	fixture.reset()
	page := fixture.list(t, ListOptions{Limit: 10})
	if len(page) != 10 {
		t.Fatalf("page = %d facts", len(page))
	}
	t.Logf("page of 10: %d payload reads, %d merge-stream reads", fixture.kv.gets, fixture.log.reads)
	if fixture.kv.gets != 10+generationPointer || fixture.log.reads != 10 {
		t.Fatalf("a page of 10 read %d payloads and %d merge streams", fixture.kv.gets, fixture.log.reads)
	}
	// A page from the middle of the conversation: the same ten, and again only
	// the reads they need.
	fixture.reset()
	start := whole[29]
	middle := fixture.list(t, ListOptions{
		Limit: 10, AfterCreatedAt: start.CreatedAt, AfterID: start.ID,
	})
	t.Logf("page of 10 after fact 30: %d payload reads, %d merge-stream reads", fixture.kv.gets, fixture.log.reads)
	if !reflect.DeepEqual(fixture.ids(middle), fixture.ids(whole[30:40])) {
		t.Fatalf("page after fact 30 = %v", fixture.ids(middle))
	}
	if fixture.kv.gets != 10+generationPointer || fixture.log.reads != 10 {
		t.Fatalf("a page after fact 30 read %d payloads and %d merge streams", fixture.kv.gets, fixture.log.reads)
	}
	// Paging through the whole conversation returns every fact once, in the
	// order a whole read returns them.
	options := ListOptions{Limit: 10}
	var walked []string
	for {
		page := fixture.list(t, options)
		if len(page) == 0 {
			break
		}
		walked = append(walked, fixture.ids(page)...)
		last := page[len(page)-1]
		options.AfterCreatedAt, options.AfterID = last.CreatedAt, last.ID
	}
	if !reflect.DeepEqual(walked, fixture.ids(whole)) {
		t.Fatalf("paging returned %v\nwant %v", walked, fixture.ids(whole))
	}
}

// TestListReturnsAFactItCannotPlace pins the answer for a fact whose header
// cannot be read: it has no place in the order a cursor pages through, so a
// page returns it and the read reports what is wrong instead of walking past
// it.
func TestListReturnsAFactItCannotPlace(t *testing.T) {
	fixture := newPageFixture(t, 4)
	ctx := context.Background()
	key, err := fixture.store.factKey(factScope, "conversation", factGeneration, "fact-03")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.kv.Put(ctx, key, []byte("{not a fact")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.List(ctx, factScope, "conversation", ListOptions{}); err == nil {
		t.Fatal("a fact that cannot be read was skipped")
	} else if !strings.Contains(err.Error(), "fact-03") {
		t.Fatalf("unreadable fact error = %v", err)
	}
	// A page that does not reach it reads the facts that come before it, and a
	// page that reaches it reports it: no cursor can place a fact whose header
	// is unreadable, so it is the last one a reader meets.
	start := fixture.list(t, ListOptions{Limit: 2})
	if got := fixture.ids(start); !reflect.DeepEqual(got, []string{"fact-00", "fact-01"}) {
		t.Fatalf("first page = %v", got)
	}
	cursor := start[len(start)-1]
	if _, err := fixture.store.List(ctx, factScope, "conversation", ListOptions{
		Limit: 2, AfterCreatedAt: cursor.CreatedAt, AfterID: cursor.ID,
	}); err == nil || !strings.Contains(err.Error(), "fact-03") {
		t.Fatalf("a page reaching the unreadable fact = %v", err)
	}
}
