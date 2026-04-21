package knowledgeproc

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"
)

// ------------------------------------------------------------------ //
// test doubles
// ------------------------------------------------------------------ //

// stubStore implements just enough of model.Store for the worker. Any
// method not defined here delegates to the embedded nil interface and
// will panic so an accidental dependency surfaces immediately.
type stubStore struct {
	model.Store

	mu              sync.Mutex
	patchHistory    map[string][]model.DocumentStatsPatch // by docID
	docsByDataset   map[string][]*model.DatasetDocument   // ListDocuments source of truth
	datasetAbstract map[string]string                     // last abstract written per datasetID
	abstractCalls   int32
	abstractErr     error
}

func newStubStore() *stubStore {
	return &stubStore{
		patchHistory:    map[string][]model.DocumentStatsPatch{},
		docsByDataset:   map[string][]*model.DatasetDocument{},
		datasetAbstract: map[string]string{},
	}
}

func (s *stubStore) seedDoc(datasetID string, doc *model.DatasetDocument) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docsByDataset[datasetID] = append(s.docsByDataset[datasetID], doc)
}

func (s *stubStore) ListDocuments(_ context.Context, datasetID string) ([]*model.DatasetDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	docs := s.docsByDataset[datasetID]
	out := make([]*model.DatasetDocument, len(docs))
	for i, d := range docs {
		dc := *d
		// Surface the latest L0Abstract patched by the worker so the
		// rollup observes fresh abstracts.
		if hist := s.patchHistory[d.ID]; len(hist) > 0 {
			for j := len(hist) - 1; j >= 0; j-- {
				if hist[j].L0Abstract != nil {
					dc.L0Abstract = *hist[j].L0Abstract
					break
				}
			}
		}
		out[i] = &dc
	}
	return out, nil
}

func (s *stubStore) UpdateDatasetAbstract(_ context.Context, datasetID, abstract string) error {
	if s.abstractErr != nil {
		return s.abstractErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.datasetAbstract[datasetID] = abstract
	atomic.AddInt32(&s.abstractCalls, 1)
	return nil
}

func (s *stubStore) datasetAbstractCalls() int { return int(atomic.LoadInt32(&s.abstractCalls)) }

func (s *stubStore) UpdateDocumentStats(_ context.Context, _, docID string, patch model.DocumentStatsPatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patchHistory[docID] = append(s.patchHistory[docID], patch)
	return nil
}

func (s *stubStore) statusHistory(docID string) []model.ProcessingStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	hist := s.patchHistory[docID]
	out := make([]model.ProcessingStatus, 0, len(hist))
	for _, p := range hist {
		if p.ProcessingStatus != nil {
			out = append(out, *p.ProcessingStatus)
		}
	}
	return out
}

func (s *stubStore) lastPatch(docID string) (model.DocumentStatsPatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hist := s.patchHistory[docID]
	if len(hist) == 0 {
		return model.DocumentStatsPatch{}, false
	}
	return hist[len(hist)-1], true
}

// stubLLM is a minimal llm.LLM that returns canned per-prompt
// responses keyed off well-known phrases in the prompt body.
type stubLLM struct {
	abstractResp        string
	abstractErr         error
	overviewResp        string
	overviewErr         error
	datasetOverviewResp string
	datasetOverviewErr  error
	delay               time.Duration
	calls               int32
}

func (s *stubLLM) Generate(ctx context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	atomic.AddInt32(&s.calls, 1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return llm.Message{}, llm.TokenUsage{}, ctx.Err()
		}
	}
	if len(msgs) == 0 {
		return llm.Message{}, llm.TokenUsage{}, errors.New("empty prompt")
	}
	body := msgs[0].Content()
	switch {
	case strings.Contains(body, "Document summaries:"):
		if s.datasetOverviewErr != nil {
			return llm.Message{}, llm.TokenUsage{}, s.datasetOverviewErr
		}
		return llm.NewTextMessage(llm.RoleAssistant, s.datasetOverviewResp), llm.TokenUsage{}, nil
	case strings.Contains(body, "structured overview"):
		if s.overviewErr != nil {
			return llm.Message{}, llm.TokenUsage{}, s.overviewErr
		}
		return llm.NewTextMessage(llm.RoleAssistant, s.overviewResp), llm.TokenUsage{}, nil
	default:
		if s.abstractErr != nil {
			return llm.Message{}, llm.TokenUsage{}, s.abstractErr
		}
		return llm.NewTextMessage(llm.RoleAssistant, s.abstractResp), llm.TokenUsage{}, nil
	}
}

func (s *stubLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("not implemented")
}

func (s *stubLLM) callCount() int { return int(atomic.LoadInt32(&s.calls)) }

// newWorker builds a Worker with rollup disabled.
func newWorker(t *testing.T, ll llm.LLM) (*Worker, *knowledge.FSStore, *stubStore) {
	return newWorkerWithRollup(t, ll, -1)
}

func newWorkerWithRollup(t *testing.T, ll llm.LLM, debounce time.Duration) (*Worker, *knowledge.FSStore, *stubStore) {
	t.Helper()
	ws := workspace.NewMemWorkspace()
	fs := knowledge.NewFSStore(ws)
	if err := fs.BuildIndex(context.Background()); err != nil {
		t.Fatalf("build index: %v", err)
	}
	cs := knowledge.NewCachedStore(fs)
	store := newStubStore()
	w, err := New(Deps{
		FSStore:        fs,
		CachedStore:    cs,
		AppStore:       store,
		LLM:            ll,
		Concurrency:    2,
		QueueSize:      8,
		JobTimeout:     2 * time.Second,
		RollupDebounce: debounce,
		RollupTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	return w, fs, store
}

func addRawDoc(t *testing.T, fs *knowledge.FSStore, datasetID, name, body string) {
	t.Helper()
	if err := fs.AddDocument(context.Background(), datasetID, name, body); err != nil {
		t.Fatalf("add raw doc: %v", err)
	}
}

// ------------------------------------------------------------------ //
// constructor validation
// ------------------------------------------------------------------ //

func TestNew_RequiresLLM(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	fs := knowledge.NewFSStore(ws)
	store := newStubStore()
	if _, err := New(Deps{FSStore: fs, AppStore: store}); err == nil {
		t.Fatal("expected error when LLM is nil")
	}
	if _, err := New(Deps{FSStore: fs, LLM: &stubLLM{}}); err == nil {
		t.Fatal("expected error when AppStore is nil")
	}
	if _, err := New(Deps{AppStore: store, LLM: &stubLLM{}}); err == nil {
		t.Fatal("expected error when FSStore is nil")
	}
}

// ------------------------------------------------------------------ //
// document lifecycle tests
// ------------------------------------------------------------------ //

func TestWorker_HappyPath_PersistsCompletedContext(t *testing.T) {
	stub := &stubLLM{abstractResp: "L0 summary", overviewResp: "L1 overview"}
	w, fs, store := newWorker(t, stub)
	addRawDoc(t, fs, "ds", "doc.md", "body")

	w.Start(context.Background())
	defer w.Stop()

	if err := w.SubmitDocument(context.Background(), "ds", "doc-1", "doc.md", "body"); err != nil {
		t.Fatalf("submit: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		hist := store.statusHistory("doc-1")
		return len(hist) > 0 && hist[len(hist)-1] == model.ProcessingCompleted
	})

	if got, _ := fs.Abstract(context.Background(), "ds", "doc.md"); got != "L0 summary" {
		t.Fatalf("FSStore abstract: %q", got)
	}
	if got, _ := fs.Overview(context.Background(), "ds", "doc.md"); got != "L1 overview" {
		t.Fatalf("FSStore overview: %q", got)
	}
	final, _ := store.lastPatch("doc-1")
	if final.L0Abstract == nil || *final.L0Abstract != "L0 summary" {
		t.Fatalf("app store L0: %+v", final.L0Abstract)
	}
	if final.L1Overview == nil || *final.L1Overview != "L1 overview" {
		t.Fatalf("app store L1: %+v", final.L1Overview)
	}
}

func TestWorker_LLMFailure_MarksFailedAndKeepsPartial(t *testing.T) {
	stub := &stubLLM{abstractResp: "kept", overviewErr: errors.New("boom")}
	w, fs, store := newWorker(t, stub)
	addRawDoc(t, fs, "ds", "doc.md", "body")

	w.Start(context.Background())
	defer w.Stop()

	_ = w.SubmitDocument(context.Background(), "ds", "doc-1", "doc.md", "body")

	waitFor(t, time.Second, func() bool {
		hist := store.statusHistory("doc-1")
		return len(hist) > 0 && hist[len(hist)-1] == model.ProcessingFailed
	})

	final, _ := store.lastPatch("doc-1")
	if final.L0Abstract == nil || *final.L0Abstract != "kept" {
		t.Fatalf("partial L0 should be kept on overview failure: %+v", final.L0Abstract)
	}
	if final.L1Overview != nil {
		t.Fatalf("L1 should be empty on overview failure: %+v", final.L1Overview)
	}
}

func TestWorker_Concurrent_AllDocsSettle(t *testing.T) {
	stub := &stubLLM{abstractResp: "abs", overviewResp: "ov"}
	w, fs, store := newWorker(t, stub)
	const n = 10
	for i := 0; i < n; i++ {
		addRawDoc(t, fs, "ds", docName(i), "body")
	}
	w.Start(context.Background())
	defer w.Stop()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.SubmitDocument(context.Background(), "ds", docID(i), docName(i), "body")
		}()
	}
	wg.Wait()

	waitFor(t, 3*time.Second, func() bool {
		for i := 0; i < n; i++ {
			hist := store.statusHistory(docID(i))
			if len(hist) == 0 || hist[len(hist)-1] != model.ProcessingCompleted {
				return false
			}
		}
		return true
	})

	if got, want := stub.callCount(), n*2; got != want {
		t.Fatalf("LLM calls: got %d want %d", got, want)
	}
}

func TestWorker_Cancel_AbortsInflightJob(t *testing.T) {
	stub := &stubLLM{abstractResp: "abs", overviewResp: "ov", delay: 200 * time.Millisecond}
	w, fs, store := newWorker(t, stub)
	addRawDoc(t, fs, "ds", "doc.md", "body")

	w.Start(context.Background())
	defer w.Stop()

	_ = w.SubmitDocument(context.Background(), "ds", "doc-1", "doc.md", "body")
	time.Sleep(50 * time.Millisecond)
	w.Cancel("doc-1")

	waitFor(t, time.Second, func() bool {
		hist := store.statusHistory("doc-1")
		return len(hist) > 0 && hist[len(hist)-1] == model.ProcessingFailed
	})
}

func TestWorker_Cancel_QueuedJob_TombstoneDrops(t *testing.T) {
	// Tiny pool with a slow first job so the second one queues; we
	// cancel the queued one and assert it is never started by checking
	// LLM call count.
	stub := &stubLLM{abstractResp: "abs", overviewResp: "ov", delay: 200 * time.Millisecond}
	w, fs, store := newWorkerWithRollup(t, stub, -1)
	addRawDoc(t, fs, "ds", "doc-a.md", "body")
	addRawDoc(t, fs, "ds", "doc-b.md", "body")

	w.deps.Concurrency = 1 // serialize so doc-b queues behind doc-a
	w.Start(context.Background())
	defer w.Stop()

	_ = w.SubmitDocument(context.Background(), "ds", "doc-a", "doc-a.md", "body")
	_ = w.SubmitDocument(context.Background(), "ds", "doc-b", "doc-b.md", "body")
	w.Cancel("doc-b")

	waitFor(t, 2*time.Second, func() bool {
		hist := store.statusHistory("doc-a")
		return len(hist) > 0 && hist[len(hist)-1] == model.ProcessingCompleted
	})

	hist := store.statusHistory("doc-b")
	for _, st := range hist {
		if st == model.ProcessingCompleted {
			t.Fatalf("cancelled queued job should not complete: %+v", hist)
		}
	}
}

func TestWorker_Stop_DrainsAndUnblocks(t *testing.T) {
	stub := &stubLLM{abstractResp: "abs", overviewResp: "ov", delay: 100 * time.Millisecond}
	w, fs, _ := newWorker(t, stub)
	addRawDoc(t, fs, "ds", "doc.md", "body")
	w.Start(context.Background())

	_ = w.SubmitDocument(context.Background(), "ds", "doc-1", "doc.md", "body")

	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s")
	}

	w.Stop()
}

// ------------------------------------------------------------------ //
// dataset rollup tests
// ------------------------------------------------------------------ //

func TestWorker_DatasetRollup_DebouncesBurst(t *testing.T) {
	stub := &stubLLM{
		abstractResp:        "doc-l0",
		overviewResp:        "doc-l1",
		datasetOverviewResp: "ds-l1",
	}
	w, fs, store := newWorkerWithRollup(t, stub, 80*time.Millisecond)
	const n = 5
	for i := 0; i < n; i++ {
		addRawDoc(t, fs, "ds", docName(i), "body")
		store.seedDoc("ds", &model.DatasetDocument{ID: docID(i), Name: docName(i)})
	}
	w.Start(context.Background())
	defer w.Stop()

	for i := 0; i < n; i++ {
		_ = w.SubmitDocument(context.Background(), "ds", docID(i), docName(i), "body")
	}

	waitFor(t, 3*time.Second, func() bool {
		return store.datasetAbstractCalls() >= 1 && store.datasetAbstract["ds"] != ""
	})

	time.Sleep(150 * time.Millisecond)
	if got := store.datasetAbstractCalls(); got != 1 {
		t.Fatalf("expected exactly 1 rollup, got %d", got)
	}
	if store.datasetAbstract["ds"] != "doc-l0" {
		t.Fatalf("dataset L0: %q", store.datasetAbstract["ds"])
	}
	if got, _ := fs.DatasetOverview(context.Background(), "ds"); got != "ds-l1" {
		t.Fatalf("FSStore dataset overview: %q", got)
	}
}

func TestWorker_DatasetRollup_EmptyAbstractsSkipsLLM(t *testing.T) {
	stub := &stubLLM{abstractResp: "abs", overviewResp: "ov"}
	w, _, store := newWorkerWithRollup(t, stub, 30*time.Millisecond)
	store.seedDoc("ds", &model.DatasetDocument{ID: "d-1", Name: "doc.md"})
	w.Start(context.Background())
	defer w.Stop()

	w.scheduleDatasetRollup("ds")
	time.Sleep(120 * time.Millisecond)

	if got := store.datasetAbstractCalls(); got != 0 {
		t.Fatalf("expected 0 rollup writes for empty abstracts, got %d", got)
	}
}

func TestWorker_DatasetRollup_PersistFailureLeavesFSStoreSet(t *testing.T) {
	stub := &stubLLM{
		abstractResp:        "doc-l0",
		overviewResp:        "doc-l1",
		datasetOverviewResp: "ds-l1",
	}
	w, fs, store := newWorkerWithRollup(t, stub, 30*time.Millisecond)
	addRawDoc(t, fs, "ds", "doc.md", "body")
	store.seedDoc("ds", &model.DatasetDocument{ID: "d-1", Name: "doc.md", L0Abstract: "seed-l0"})
	store.abstractErr = errors.New("simulated dataset abstract write failure")

	w.Start(context.Background())
	defer w.Stop()

	w.scheduleDatasetRollup("ds")
	time.Sleep(150 * time.Millisecond)

	if got, _ := fs.DatasetAbstract(context.Background(), "ds"); got == "" {
		t.Fatal("FSStore dataset abstract should be set even when app store write fails")
	}
}

// stopWaitsForRollup verifies that Stop blocks until any in-flight
// debounced rollup goroutine has exited.
func TestWorker_Stop_WaitsForRollup(t *testing.T) {
	stub := &stubLLM{abstractResp: "abs", overviewResp: "ov", datasetOverviewResp: "ds-ov", delay: 100 * time.Millisecond}
	w, fs, store := newWorkerWithRollup(t, stub, 20*time.Millisecond)
	addRawDoc(t, fs, "ds", "doc.md", "body")
	store.seedDoc("ds", &model.DatasetDocument{ID: "d-1", Name: "doc.md", L0Abstract: "seed"})
	w.Start(context.Background())

	w.scheduleDatasetRollup("ds")
	// Give the debounce timer a chance to fire and the rollup to begin.
	time.Sleep(60 * time.Millisecond)

	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not wait for rollup to finish")
	}
}

func TestWorker_SubmitBeforeStart_Errors(t *testing.T) {
	stub := &stubLLM{abstractResp: "abs", overviewResp: "ov"}
	w, _, _ := newWorker(t, stub)
	if err := w.SubmitDocument(context.Background(), "ds", "doc-1", "doc.md", "body"); err == nil {
		t.Fatal("expected error when submitting before Start")
	}
}

// ------------------------------------------------------------------ //
// helpers
// ------------------------------------------------------------------ //

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func docName(i int) string { return "doc-" + strconv.Itoa(i) + ".md" }
func docID(i int) string   { return "id-" + strconv.Itoa(i) }
