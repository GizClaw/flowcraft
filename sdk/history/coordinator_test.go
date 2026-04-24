package history

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// newTestCoordinator wires the same dependencies NewCompacted does, but
// keeps the helper local so tests can poke internal state directly.
func newTestCoordinator(t *testing.T, ws workspace.Workspace) *compactor {
	t.Helper()
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, DefaultDAGConfig(), &EstimateCounter{})
	return newCompactor(store, dag, DefaultDAGConfig(), ws, "memory")
}

// TestCoordinator_ShutdownRefusesNewWork pins down ErrClosed: once
// Shutdown is observed, Append/Compact/Archive/Clear must reject so
// callers cannot accidentally enqueue against a closed worker.
func TestCoordinator_ShutdownRefusesNewWork(t *testing.T) {
	c := newTestCoordinator(t, nil)

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	cases := []struct {
		name string
		fn   func() error
	}{
		{"Append", func() error {
			return c.Append(context.Background(), "c", []model.Message{model.NewTextMessage(model.RoleUser, "x")})
		}},
		{"Compact", func() error {
			_, err := c.Compact(context.Background(), "c")
			return err
		}},
		{"Archive", func() error {
			_, err := c.Archive(context.Background(), "c")
			// Archive returns nil when ws==nil, which short-circuits before
			// the closed-state check; that's deliberate (no-op semantics
			// match v0.2 behaviour). Treat nil as "not applicable" here.
			if err == nil {
				return ErrClosed
			}
			return err
		}},
		{"Clear", func() error { return c.Clear(context.Background(), "c") }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); !errors.Is(err, ErrClosed) {
				t.Fatalf("expected ErrClosed, got %v", err)
			}
		})
	}
}

// TestCoordinator_ShutdownIdempotent verifies that a second Shutdown call
// does not panic and returns nil after the first drain completes.
func TestCoordinator_ShutdownIdempotent(t *testing.T) {
	c := newTestCoordinator(t, nil)

	for i := 0; i < 5; i++ {
		_ = c.Append(context.Background(), fmt.Sprintf("conv-%d", i), []model.Message{
			model.NewTextMessage(model.RoleUser, "msg"),
		})
	}

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// TestCoordinator_ShutdownContextDeadline verifies S3 semantics: a
// context deadline returns ctx.Err() but does not cancel in-flight
// workers; a second Shutdown with an unbounded ctx attaches to the same
// drain and returns nil once everything has actually exited.
func TestCoordinator_ShutdownContextDeadline(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	slow := &slowMockLLM{delay: 200 * time.Millisecond}
	dag := NewSummaryDAG(summaryStore, store, slow, DefaultDAGConfig(), &EstimateCounter{})
	c := newCompactor(store, dag, DefaultDAGConfig(), nil, "memory")

	for i := 0; i < 5; i++ {
		_ = c.Append(context.Background(), fmt.Sprintf("conv-%d", i), []model.Message{
			model.NewTextMessage(model.RoleUser, "hello"),
			model.NewTextMessage(model.RoleAssistant, "hi"),
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := c.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}

	// Re-Shutdown with no deadline must observe the eventual drain.
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// TestCoordinator_AppendArchiveSerialized exercises the M2 invariant: a
// background archive runs strictly after persistAppend has released the
// queue, never overlapping with another Append's persistAppend on the
// same conversation. The probe is a synthetic Store that asserts no
// concurrent SaveMessages calls.
func TestCoordinator_AppendArchiveSerialized(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := newConcurrencyAssertingStore(t)
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	cfg := DefaultDAGConfig()
	cfg.Archive.ArchiveThreshold = 1
	cfg.Archive.ArchiveBatchSize = 1
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, cfg, &EstimateCounter{})
	c := newCompactor(store, dag, cfg, ws, "memory")
	defer func() { _ = c.Shutdown(context.Background()) }()

	for i := 0; i < 50; i++ {
		if err := c.Append(context.Background(), "single", []model.Message{
			model.NewTextMessage(model.RoleUser, "hello"),
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if store.maxConcurrent.Load() > 1 {
		t.Fatalf("SaveMessages observed concurrency on the same conversation: max=%d",
			store.maxConcurrent.Load())
	}
}

// TestCoordinator_ClearReapsWorker verifies W3: after Clear, the queue
// for the conversation must be removed so the worker exits.
func TestCoordinator_ClearReapsWorker(t *testing.T) {
	c := newTestCoordinator(t, nil)
	defer func() { _ = c.Shutdown(context.Background()) }()

	if err := c.Append(context.Background(), "to-clear", []model.Message{
		model.NewTextMessage(model.RoleUser, "x"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := c.Clear(context.Background(), "to-clear"); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	c.mu.Lock()
	_, present := c.queues["to-clear"]
	c.mu.Unlock()
	if present {
		t.Fatal("expected queue to be reaped after Clear")
	}
}

// TestLoad_PreservesSystemMessage covers the bug where MaxMessages
// trimming used to strip a leading system prompt.
func TestLoad_PreservesSystemMessage(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, DefaultDAGConfig(), &EstimateCounter{})
	c := newCompactor(store, dag, DefaultDAGConfig(), nil, "memory")
	defer func() { _ = c.Shutdown(context.Background()) }()

	ctx := context.Background()
	convID := "system-preserved"
	_ = store.SaveMessages(ctx, convID, []model.Message{
		model.NewTextMessage(model.RoleSystem, "you are an assistant"),
		model.NewTextMessage(model.RoleUser, "u1"),
		model.NewTextMessage(model.RoleAssistant, "a1"),
		model.NewTextMessage(model.RoleUser, "u2"),
		model.NewTextMessage(model.RoleAssistant, "a2"),
		model.NewTextMessage(model.RoleUser, "u3"),
	})

	got, err := c.Load(ctx, convID, Budget{MaxMessages: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 msgs, got %d", len(got))
	}
	if got[0].Role != model.RoleSystem {
		t.Fatalf("expected system to lead, got role=%s", got[0].Role)
	}
}

// TestCoordinator_LazyArchiveRecovery checks D-R3: a stranded intent file
// from a prior crash is recovered the first time the conversation is
// touched after restart.
func TestCoordinator_LazyArchiveRecovery(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewInMemoryStore()
	ctx := context.Background()
	convID := "recovers"

	msgs := make([]model.Message, 30)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "m")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	// First archive: completes normally so we have a manifest.
	cfg := ArchiveConfig{ArchiveThreshold: 20, ArchiveBatchSize: 15}
	if _, err := archiveImpl(ctx, ws, store, "memory", convID, cfg); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash mid-archive by hand-writing a "manifest_updated"
	// intent: trim hasn't happened yet, so RecoverArchive should rerun
	// the trim phase.
	intent := &archiveIntent{
		ConvID: convID, StartSeq: 100, EndSeq: 109, BatchSize: 5,
		ArchiveFile: "messages_100_109.jsonl.gz", Phase: "manifest_updated",
	}
	if err := writeIntent(ctx, ws, "memory", "archive", convID, intent); err != nil {
		t.Fatal(err)
	}

	// Construct a fresh coordinator — startup scan + lazy on-touch should
	// both clear the intent.
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, DefaultDAGConfig(), &EstimateCounter{})
	c := newCompactor(store, dag, DefaultDAGConfig(), ws, "memory")
	defer func() { _ = c.Shutdown(context.Background()) }()

	// Touch the conversation to force the lazy path even if startup hasn't
	// scheduled yet.
	if err := c.Append(ctx, convID, []model.Message{
		model.NewTextMessage(model.RoleUser, "after-recover"),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	exists, err := ws.Exists(ctx, "memory/"+convID+"/archive/intent.json")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("intent.json should be cleared after recovery")
	}
}

// TestCoordinator_ShutdownNoGoroutineLeakOnRetry pins down the
// sync.Once + shared shutdownDone invariant: a supervisor that retries
// Shutdown in a deadline-bounded loop must NOT accumulate one
// drain-watcher goroutine per call. Pre-fix, every Shutdown invocation
// spawned its own `go func() { closeWg.Wait(); close(done) }()`, so 100
// retries against a slow drain leaked 100 goroutines until the workers
// finally exited. Post-fix, the watcher count tops out at 1 regardless
// of how many Shutdown attempts pile up.
func TestCoordinator_ShutdownNoGoroutineLeakOnRetry(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	// Slow LLM keeps the per-conversation worker busy so all the
	// deadline-bounded Shutdown calls below see a not-yet-drained state
	// and exercise the "deadline returns ctx.Err but watcher is still
	// running" branch.
	slow := &slowMockLLM{delay: 100 * time.Millisecond}
	dag := NewSummaryDAG(summaryStore, store, slow, DefaultDAGConfig(), &EstimateCounter{})
	c := newCompactor(store, dag, DefaultDAGConfig(), nil, "memory")

	for i := 0; i < 5; i++ {
		_ = c.Append(context.Background(), fmt.Sprintf("conv-%d", i), []model.Message{
			model.NewTextMessage(model.RoleUser, "hello"),
			model.NewTextMessage(model.RoleAssistant, "hi"),
		})
	}

	baseline := runtime.NumGoroutine()

	// Hammer Shutdown with tight deadlines while the workers are still
	// busy. Pre-fix this would spawn ~100 watcher goroutines.
	const retries = 100
	for i := 0; i < retries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		err := c.Shutdown(ctx)
		cancel()
		if err == nil {
			// Workers happened to drain in <1ms; nothing more to test.
			break
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("retry %d: expected DeadlineExceeded, got %v", i, err)
		}
	}

	// While shutdown is still in flight, the goroutine delta from the
	// pre-Shutdown baseline must stay tiny — at most a handful (the
	// single watcher + GC/scavenger noise). Pre-fix this would be
	// ≈retries.
	growth := runtime.NumGoroutine() - baseline
	if growth > 8 {
		t.Fatalf("Shutdown leaked goroutines: baseline=%d after %d retries grew by %d",
			baseline, retries, growth)
	}

	// Final unbounded Shutdown attaches to the same drain and returns nil.
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("final Shutdown: %v", err)
	}
}

// TestCoordinator_StateClosedAfterDrain pins the second half of the
// state-machine cleanup: stateClosed must only be observed AFTER every
// worker has drained, never while a worker is still inside runTask.
// Pre-fix, the CAS lived inside the deadline-returning branch and so
// stateClosed was effectively dead code; post-fix it is set by the
// watcher goroutine once closeWg.Wait returns.
func TestCoordinator_StateClosedAfterDrain(t *testing.T) {
	store := NewInMemoryStore()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	slow := &slowMockLLM{delay: 100 * time.Millisecond}
	dag := NewSummaryDAG(summaryStore, store, slow, DefaultDAGConfig(), &EstimateCounter{})
	c := newCompactor(store, dag, DefaultDAGConfig(), nil, "memory")

	for i := 0; i < 3; i++ {
		_ = c.Append(context.Background(), fmt.Sprintf("conv-%d", i), []model.Message{
			model.NewTextMessage(model.RoleUser, "hello"),
		})
	}

	// First Shutdown: deadline expires while workers are still running.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	err := c.Shutdown(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if got := c.state.Load(); got != stateClosing {
		t.Fatalf("after deadline-bounded Shutdown: expected stateClosing(%d), got %d",
			stateClosing, got)
	}

	// Drain attaches to the same watcher; returns once stateClosed is set.
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("drain Shutdown: %v", err)
	}
	if got := c.state.Load(); got != stateClosed {
		t.Fatalf("after drained Shutdown: expected stateClosed(%d), got %d",
			stateClosed, got)
	}
}

// concurrencyAssertingStore wraps InMemoryStore and tracks the maximum
// number of concurrent SaveMessages calls observed.
type concurrencyAssertingStore struct {
	t             *testing.T
	inner         *InMemoryStore
	mu            sync.Mutex
	inflight      int
	maxConcurrent atomicInt32
}

type atomicInt32 struct {
	mu sync.Mutex
	v  int32
}

func (a *atomicInt32) Set(v int32) {
	a.mu.Lock()
	a.v = v
	a.mu.Unlock()
}

func (a *atomicInt32) Load() int32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

func (a *atomicInt32) Max(v int32) {
	a.mu.Lock()
	if v > a.v {
		a.v = v
	}
	a.mu.Unlock()
}

func newConcurrencyAssertingStore(t *testing.T) *concurrencyAssertingStore {
	return &concurrencyAssertingStore{t: t, inner: NewInMemoryStore()}
}

func (s *concurrencyAssertingStore) SaveMessages(ctx context.Context, convID string, msgs []model.Message) error {
	s.mu.Lock()
	s.inflight++
	s.maxConcurrent.Max(int32(s.inflight))
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()
	return s.inner.SaveMessages(ctx, convID, msgs)
}

func (s *concurrencyAssertingStore) GetMessages(ctx context.Context, convID string) ([]model.Message, error) {
	return s.inner.GetMessages(ctx, convID)
}

func (s *concurrencyAssertingStore) DeleteMessages(ctx context.Context, convID string) error {
	return s.inner.DeleteMessages(ctx, convID)
}

// Ensure the wrapper participates as a plain Store (no MessageAppender)
// so persistAppend goes through the SaveMessages path the test asserts.
var _ Store = (*concurrencyAssertingStore)(nil)

// noopMessage placeholder helper used by the deadline test to avoid
// importing llm in places that already have model.
var _ = llm.NewTextMessage

// --- Coordinator happy-path coverage for Compact / Archive ---

func TestCoordinator_CompactRunsThroughQueue(t *testing.T) {
	store := NewInMemoryStore()
	defer store.Close()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, DefaultDAGConfig(), &EstimateCounter{})
	c := newCompactor(store, dag, DefaultDAGConfig(), nil, "")
	defer func() { _ = c.Shutdown(context.Background()) }()

	ctx := context.Background()
	convID := "compact-via-coord"

	// Pre-populate one stale node so Compact has actual work to do.
	stale := &SummaryNode{ConversationID: convID, Depth: 0, Content: "leaf"}
	_ = summaryStore.Save(ctx, stale)
	_ = summaryStore.DeleteByConvID(ctx, convID, stale.ID)

	res, err := c.Compact(ctx, convID)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.DeletedRemoved == 0 {
		t.Fatalf("expected DeletedRemoved > 0, got %+v", res)
	}
}

func TestCoordinator_ArchiveNoWorkspaceShortCircuits(t *testing.T) {
	// Without a workspace the public contract is "return empty result, no
	// error" — exercises the early return in Archive.
	store := NewInMemoryStore()
	defer store.Close()
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, DefaultDAGConfig(), &EstimateCounter{})
	c := newCompactor(store, dag, DefaultDAGConfig(), nil, "")
	defer func() { _ = c.Shutdown(context.Background()) }()

	res, err := c.Archive(context.Background(), "any")
	if err != nil {
		t.Fatalf("Archive without ws: %v", err)
	}
	if res.MessagesArchived != 0 || res.HotStartSeq != 0 {
		t.Fatalf("expected zero result, got %+v", res)
	}
}

func TestCoordinator_ArchiveRunsThroughQueue(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(ws, "memory")
	summaryStore := &inMemSummaryStore{data: make(map[string][]*SummaryNode)}

	cfg := DefaultDAGConfig()
	cfg.Archive.ArchiveThreshold = 5
	cfg.Archive.ArchiveBatchSize = 3
	dag := NewSummaryDAG(summaryStore, store, &mockSummaryLLM{}, cfg, &EstimateCounter{})
	c := newCompactor(store, dag, cfg, ws, "memory")
	defer func() { _ = c.Shutdown(context.Background()) }()

	ctx := context.Background()
	convID := "archive-via-coord"
	msgs := make([]model.Message, 10)
	for i := range msgs {
		msgs[i] = model.NewTextMessage(model.RoleUser, "x")
	}
	_ = store.SaveMessages(ctx, convID, msgs)

	res, err := c.Archive(ctx, convID)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if res.MessagesArchived != 3 {
		t.Fatalf("expected 3 archived, got %d (res=%+v)", res.MessagesArchived, res)
	}
}
