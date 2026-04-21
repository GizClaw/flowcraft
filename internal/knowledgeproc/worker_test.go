package knowledgeproc

import (
	"context"
	"errors"
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
// other method panics so an accidental dependency surfaces immediately.
type stubStore struct {
	model.Store // embed for method-set satisfaction; nil entries panic on call

	mu              sync.Mutex
	byID            map[string]model.DocumentStatsPatch  // last patch keyed by docID
	byName          map[string]model.DocumentStatsPatch  // last patch keyed by name
	statuses        map[string][]model.ProcessingStatus  // ordered status history per name
	docsByDataset   map[string][]*model.DatasetDocument  // ListDocuments source of truth
	datasetAbstract map[string]string                    // last abstract written per datasetID
	abstractCalls   int32                                // count UpdateDatasetAbstract calls
	failOnce        bool                                 // when true, the first UpdateDocumentStats fails
	abstractErr     error                                // when set, UpdateDatasetAbstract returns this
}

func newStubStore() *stubStore {
	return &stubStore{
		byID:            map[string]model.DocumentStatsPatch{},
		byName:          map[string]model.DocumentStatsPatch{},
		statuses:        map[string][]model.ProcessingStatus{},
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
		// Return a shallow copy with the latest L0 patched in so the
		// rollup sees fresh abstracts produced by the worker.
		dc := *d
		if patch, ok := s.byName[d.Name]; ok && patch.L0Abstract != nil {
			dc.L0Abstract = *patch.L0Abstract
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
	if s.failOnce {
		s.failOnce = false
		return errors.New("simulated update failure")
	}
	s.byID[docID] = patch
	return nil
}

func (s *stubStore) UpdateDocumentStatsByName(_ context.Context, _, name string, patch model.DocumentStatsPatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byName[name] = patch
	if patch.ProcessingStatus != nil {
		s.statuses[name] = append(s.statuses[name], *patch.ProcessingStatus)
	}
	return nil
}

func (s *stubStore) statusHistory(name string) []model.ProcessingStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.ProcessingStatus, len(s.statuses[name]))
	copy(out, s.statuses[name])
	return out
}

// stubLLM is a minimal llm.LLM that returns canned per-prompt
// responses. A request whose prompt matches the L1 template (contains
// "structured overview") returns overviewResp; otherwise abstractResp.
// Optional delay simulates a slow generation so tests can race
// Cancel/Stop against in-flight jobs.
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

// newWorker builds a Worker with an in-memory FSStore + CachedStore +
// stubStore. The caller may override the LLM (pass nil for no-op mode).
// Rollup is disabled by default; tests that exercise it use newWorkerWithRollup.
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
	w := New(Deps{
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
	return w, fs, store
}

// addRawDoc seeds the FSStore so SetDoc{Abstract,Overview} have a row
// to update during the worker's persistDocContext call.
func addRawDoc(t *testing.T, fs *knowledge.FSStore, datasetID, name, body string) {
	t.Helper()
	if err := fs.AddDocument(context.Background(), datasetID, name, body); err != nil {
		t.Fatalf("add raw doc: %v", err)
	}
}

// ------------------------------------------------------------------ //
// tests
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
		hist := store.statusHistory("doc.md")
		return len(hist) > 0 && hist[len(hist)-1] == model.ProcessingCompleted
	})

	if got, _ := fs.Abstract(context.Background(), "ds", "doc.md"); got != "L0 summary" {
		t.Fatalf("FSStore abstract: %q", got)
	}
	if got, _ := fs.Overview(context.Background(), "ds", "doc.md"); got != "L1 overview" {
		t.Fatalf("FSStore overview: %q", got)
	}

	final := store.byName["doc.md"]
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
		hist := store.statusHistory("doc.md")
		return len(hist) > 0 && hist[len(hist)-1] == model.ProcessingFailed
	})

	final := store.byName["doc.md"]
	if final.L0Abstract == nil || *final.L0Abstract != "kept" {
		t.Fatalf("partial L0 should be kept on overview failure: %+v", final.L0Abstract)
	}
	if final.L1Overview != nil {
		t.Fatalf("L1 should be empty on overview failure: %+v", final.L1Overview)
	}
}

func TestWorker_NoLLM_ImmediatelyMarksCompleted(t *testing.T) {
	w, _, store := newWorker(t, nil)
	w.Start(context.Background())
	defer w.Stop()

	if w.Enabled() {
		t.Fatalf("worker should not be Enabled without an LLM")
	}

	if err := w.SubmitDocument(context.Background(), "ds", "doc-1", "doc.md", "body"); err != nil {
		t.Fatalf("submit: %v", err)
	}

	patch, ok := store.byID["doc-1"]
	if !ok || patch.ProcessingStatus == nil || *patch.ProcessingStatus != model.ProcessingCompleted {
		t.Fatalf("expected immediate completed patch, got %+v ok=%v", patch, ok)
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
			hist := store.statusHistory(docName(i))
			if len(hist) == 0 || hist[len(hist)-1] != model.ProcessingCompleted {
				return false
			}
		}
		return true
	})

	// 2 LLM calls (L0 + L1) per doc.
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
	// Give the worker a moment to pick up the job and register a cancel.
	time.Sleep(50 * time.Millisecond)
	w.Cancel("doc-1")

	waitFor(t, time.Second, func() bool {
		hist := store.statusHistory("doc.md")
		return len(hist) > 0 && hist[len(hist)-1] == model.ProcessingFailed
	})
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

	// Calling Stop again must be a no-op.
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
		store.seedDoc("ds", &model.DatasetDocument{Name: docName(i)})
	}
	w.Start(context.Background())
	defer w.Stop()

	for i := 0; i < n; i++ {
		_ = w.SubmitDocument(context.Background(), "ds", docID(i), docName(i), "body")
	}

	waitFor(t, 3*time.Second, func() bool {
		return store.datasetAbstractCalls() >= 1 && store.datasetAbstract["ds"] != ""
	})

	// Allow any straggler timer to fire.
	time.Sleep(150 * time.Millisecond)
	if got := store.datasetAbstractCalls(); got != 1 {
		t.Fatalf("expected exactly 1 rollup, got %d", got)
	}
	// dataset L0 from the AbstractPrompt second call.
	if store.datasetAbstract["ds"] != "doc-l0" {
		t.Fatalf("dataset L0: %q", store.datasetAbstract["ds"])
	}
	// FSStore in-memory dataset overview should be set as well.
	if got, _ := fs.DatasetOverview(context.Background(), "ds"); got != "ds-l1" {
		t.Fatalf("FSStore dataset overview: %q", got)
	}
}

func TestWorker_DatasetRollup_EmptyAbstractsSkipsLLM(t *testing.T) {
	stub := &stubLLM{abstractResp: "abs", overviewResp: "ov"}
	w, _, store := newWorkerWithRollup(t, stub, 30*time.Millisecond)
	// Seed a row but with no L0Abstract (and never produce one) so the
	// rollup has nothing to summarize.
	store.seedDoc("ds", &model.DatasetDocument{Name: "doc.md"})
	w.Start(context.Background())
	defer w.Stop()

	w.scheduleDatasetRollup("ds")
	time.Sleep(120 * time.Millisecond)

	if got := store.datasetAbstractCalls(); got != 0 {
		t.Fatalf("expected 0 rollup writes for empty abstracts, got %d", got)
	}
}

func TestWorker_DatasetRollup_PersistFailureLogs(t *testing.T) {
	stub := &stubLLM{
		abstractResp:        "doc-l0",
		overviewResp:        "doc-l1",
		datasetOverviewResp: "ds-l1",
	}
	w, fs, store := newWorkerWithRollup(t, stub, 30*time.Millisecond)
	addRawDoc(t, fs, "ds", "doc.md", "body")
	store.seedDoc("ds", &model.DatasetDocument{Name: "doc.md", L0Abstract: "seed-l0"})
	store.abstractErr = errors.New("simulated dataset abstract write failure")

	w.Start(context.Background())
	defer w.Stop()

	w.scheduleDatasetRollup("ds")
	time.Sleep(150 * time.Millisecond)

	// FSStore should still be populated even though the app store
	// write failed (best-effort).
	if got, _ := fs.DatasetAbstract(context.Background(), "ds"); got == "" {
		t.Fatal("FSStore dataset abstract should be set even when app store write fails")
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

func docName(i int) string { return "doc-" + itoa(i) + ".md" }
func docID(i int) string   { return "id-" + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
