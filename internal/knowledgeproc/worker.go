// Package knowledgeproc owns the platform-side orchestration that the
// sdkx knowledge SDK no longer ships. It consumes per-document jobs,
// calls knowledge.GenerateDocumentContext, and persists the result to
// both the FSStore (so retrieval and cross-dataset L0 search keep
// working) and the platform's app store (so REST DatasetDocument rows
// reflect lifecycle and layered context).
//
// The worker is intentionally in-memory: there is no persistent queue.
// Bootstrap re-submits any document still in pending/processing on
// startup via recoverPendingKnowledgeDocs.
package knowledgeproc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"

	otellog "go.opentelemetry.io/otel/log"
)

// Defaults for the worker pool.
const (
	defaultConcurrency    = 3
	defaultQueueSize      = 256
	defaultJobTimeout     = 5 * time.Minute
	defaultRollupDebounce = 30 * time.Second
	defaultRollupTimeout  = 5 * time.Minute
)

// Deps groups the worker's collaborators. All four core stores plus an
// LLM are required: the worker is the platform's only path for LLM
// driven layered-context generation, so refusing to construct one when
// any collaborator is missing keeps the failure mode at startup
// instead of silently degrading at runtime.
type Deps struct {
	FSStore     *knowledge.FSStore
	CachedStore *knowledge.CachedStore
	AppStore    model.Store
	LLM         llm.LLM

	// Concurrency controls the number of worker goroutines. Defaults to
	// 3 when zero.
	Concurrency int
	// QueueSize bounds the in-memory submit channel. Defaults to 256
	// when zero. SubmitDocument blocks on a full queue.
	QueueSize int
	// JobTimeout caps per-document LLM work. Defaults to 5 minutes when
	// zero. Set negative to disable.
	JobTimeout time.Duration
	// RollupDebounce is the quiet window after the last document
	// completion before a dataset-level rollup runs. Defaults to 30s
	// when zero. Set negative to disable rollup entirely.
	RollupDebounce time.Duration
	// RollupTimeout caps per-rollup LLM work. Defaults to 5 minutes
	// when zero. Set negative to disable.
	RollupTimeout time.Duration
}

// job describes one unit of LLM-driven context generation.
type job struct {
	datasetID string
	docID     string
	docName   string
	content   string
}

// Worker orchestrates layered-context generation for documents.
type Worker struct {
	deps Deps

	jobs chan job

	mu           sync.Mutex
	cancels      map[string]context.CancelFunc // keyed by docID
	rollupTimers map[string]*time.Timer        // keyed by datasetID
	tombstones   map[string]struct{}           // keyed by docID; in-queue jobs to drop
	started      bool
	stopping     bool

	rootCtx    context.Context
	rootCancel context.CancelFunc

	wg sync.WaitGroup
}

// New constructs a Worker. It returns an error if any required
// collaborator is missing so misconfiguration is loud at startup.
func New(deps Deps) (*Worker, error) {
	if deps.FSStore == nil {
		return nil, errors.New("knowledgeproc: FSStore is required")
	}
	if deps.AppStore == nil {
		return nil, errors.New("knowledgeproc: AppStore is required")
	}
	if deps.LLM == nil {
		return nil, errors.New("knowledgeproc: LLM is required")
	}
	if deps.Concurrency <= 0 {
		deps.Concurrency = defaultConcurrency
	}
	if deps.QueueSize <= 0 {
		deps.QueueSize = defaultQueueSize
	}
	if deps.JobTimeout == 0 {
		deps.JobTimeout = defaultJobTimeout
	}
	if deps.RollupDebounce == 0 {
		deps.RollupDebounce = defaultRollupDebounce
	}
	if deps.RollupTimeout == 0 {
		deps.RollupTimeout = defaultRollupTimeout
	}
	return &Worker{
		deps:         deps,
		jobs:         make(chan job, deps.QueueSize),
		cancels:      make(map[string]context.CancelFunc),
		rollupTimers: make(map[string]*time.Timer),
		tombstones:   make(map[string]struct{}),
	}, nil
}

// Start launches the worker pool. The supplied context bounds the
// lifetime of every in-flight job; cancelling it (or calling Stop)
// drains the queue.
func (w *Worker) Start(ctx context.Context) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.rootCtx, w.rootCancel = context.WithCancel(ctx)
	conc := w.deps.Concurrency
	w.mu.Unlock()

	for i := 0; i < conc; i++ {
		w.wg.Add(1)
		go w.loop()
	}
}

// Stop closes the queue, cancels all in-flight jobs and pending
// rollups and blocks until every goroutine has exited. Safe to call
// multiple times.
func (w *Worker) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.started || w.stopping {
		w.mu.Unlock()
		return
	}
	w.stopping = true
	close(w.jobs)
	for _, cancel := range w.cancels {
		cancel()
	}
	w.cancels = map[string]context.CancelFunc{}
	for _, t := range w.rollupTimers {
		if t.Stop() {
			// Timer had not fired yet so its wg.Done deferred in the
			// AfterFunc callback will never run; release that slot.
			w.wg.Done()
		}
	}
	w.rollupTimers = map[string]*time.Timer{}
	cancelRoot := w.rootCancel
	w.mu.Unlock()

	w.wg.Wait()
	if cancelRoot != nil {
		cancelRoot()
	}
}

// SubmitDocument transitions the document to "processing" and enqueues
// it for LLM-driven context generation. The status flip and enqueue
// are atomic from the caller's perspective: if either step fails the
// document is left in "failed" with the underlying error returned.
//
// SubmitDocument blocks on a full queue.
func (w *Worker) SubmitDocument(ctx context.Context, datasetID, docID, name, content string) error {
	if w == nil {
		return errors.New("knowledgeproc: worker is nil")
	}
	if datasetID == "" || docID == "" || name == "" {
		return errors.New("knowledgeproc: datasetID, docID and name are required")
	}

	w.mu.Lock()
	if !w.started || w.stopping {
		w.mu.Unlock()
		return errors.New("knowledgeproc: worker not running")
	}
	delete(w.tombstones, docID)
	w.mu.Unlock()

	processing := model.ProcessingRunning
	if err := w.deps.AppStore.UpdateDocumentStats(ctx, datasetID, docID, model.DocumentStatsPatch{
		ProcessingStatus: &processing,
	}); err != nil {
		return err
	}

	select {
	case w.jobs <- job{datasetID: datasetID, docID: docID, docName: name, content: content}:
		return nil
	case <-ctx.Done():
		w.markEnqueueFailed(context.Background(), datasetID, docID, ctx.Err())
		return ctx.Err()
	}
}

// Cancel best-effort aborts processing for the given docID.
//
// If the job is currently running its context is cancelled. If it is
// still queued a tombstone is recorded so the worker drops it on
// pickup. Safe to call when the worker is not running.
func (w *Worker) Cancel(docID string) {
	if w == nil || docID == "" {
		return
	}
	w.mu.Lock()
	cancel, running := w.cancels[docID]
	if running {
		delete(w.cancels, docID)
	} else {
		w.tombstones[docID] = struct{}{}
	}
	w.mu.Unlock()
	if running && cancel != nil {
		cancel()
	}
}

// loop is the per-goroutine worker body.
func (w *Worker) loop() {
	defer w.wg.Done()
	for j := range w.jobs {
		w.mu.Lock()
		_, dropped := w.tombstones[j.docID]
		if dropped {
			delete(w.tombstones, j.docID)
		}
		w.mu.Unlock()
		if dropped {
			continue
		}
		w.runJob(j)
	}
}

// runJob processes a single document end-to-end.
func (w *Worker) runJob(j job) {
	parent := w.rootCtx
	if parent == nil {
		parent = context.Background()
	}
	jobCtx, cancel := context.WithCancel(parent)
	if w.deps.JobTimeout > 0 {
		jobCtx, cancel = context.WithTimeout(parent, w.deps.JobTimeout)
	}

	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		cancel()
		return
	}
	w.cancels[j.docID] = cancel
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		delete(w.cancels, j.docID)
		w.mu.Unlock()
		cancel()
	}()

	docCtx, err := knowledge.GenerateDocumentContext(jobCtx, w.deps.LLM, j.content)
	if err != nil {
		w.markFailed(jobCtx, j, err, docCtx)
		w.scheduleDatasetRollup(j.datasetID)
		return
	}

	if err := w.persistDocContext(jobCtx, j, docCtx); err != nil {
		w.markFailed(jobCtx, j, err, docCtx)
		w.scheduleDatasetRollup(j.datasetID)
		return
	}

	completed := model.ProcessingCompleted
	patch := model.DocumentStatsPatch{
		ProcessingStatus: &completed,
		L0Abstract:       strPtr(docCtx.Abstract),
		L1Overview:       strPtr(docCtx.Overview),
	}
	if err := w.deps.AppStore.UpdateDocumentStats(jobCtx, j.datasetID, j.docID, patch); err != nil {
		telemetry.Warn(jobCtx, "knowledgeproc: persist completed status failed",
			otellog.String("dataset", j.datasetID),
			otellog.String("doc", j.docID),
			otellog.String("error", err.Error()))
	}

	w.scheduleDatasetRollup(j.datasetID)
}

// scheduleDatasetRollup arms (or re-arms) a debounced timer for a
// dataset. Bursts of completing documents collapse into a single
// rollup run. The actual run is tracked in the worker WaitGroup so
// Stop blocks until it finishes (or short-circuits when stopping).
func (w *Worker) scheduleDatasetRollup(datasetID string) {
	if w == nil || datasetID == "" {
		return
	}
	if w.deps.RollupDebounce < 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopping {
		return
	}
	// Reschedule by stopping any prior pending timer; if Stop() reports
	// the timer was active (meaning it had not fired yet) we must
	// release the matching wg.Add slot ourselves because the canceled
	// callback will never run.
	if t, ok := w.rollupTimers[datasetID]; ok {
		if t.Stop() {
			w.wg.Done()
		}
	}
	w.wg.Add(1)
	w.rollupTimers[datasetID] = time.AfterFunc(w.deps.RollupDebounce, func() {
		defer w.wg.Done()
		w.mu.Lock()
		delete(w.rollupTimers, datasetID)
		stopping := w.stopping
		w.mu.Unlock()
		if stopping {
			return
		}
		w.runDatasetRollup(datasetID)
	})
}

// runDatasetRollup aggregates per-document L0 abstracts for a dataset
// into a dataset-level L0 + L1 summary. It writes the result to both
// the FSStore (in-memory + .abstract.md / .overview.md sidecars) and
// the app store (datasets.l0_abstract). Rollups that produce zero
// non-empty abstracts skip the LLM call.
func (w *Worker) runDatasetRollup(datasetID string) {
	parent := w.rootCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	if w.deps.RollupTimeout > 0 {
		ctx, cancel = context.WithTimeout(parent, w.deps.RollupTimeout)
	}
	defer cancel()

	docs, err := w.deps.AppStore.ListDocuments(ctx, datasetID)
	if err != nil {
		telemetry.Warn(ctx, "knowledgeproc: rollup list documents failed",
			otellog.String("dataset", datasetID),
			otellog.String("error", err.Error()))
		return
	}

	summaries := make([]knowledge.DocumentSummary, 0, len(docs))
	for _, d := range docs {
		if d == nil || d.L0Abstract == "" {
			continue
		}
		summaries = append(summaries, knowledge.DocumentSummary{Name: d.Name, Abstract: d.L0Abstract})
	}
	if len(summaries) == 0 {
		return
	}

	dsCtx, genErr := knowledge.GenerateDatasetContext(ctx, w.deps.LLM, summaries)
	if genErr != nil {
		// Preserve whatever partial output the SDK managed to produce
		// (typically just an Abstract). Without it nothing was
		// generated and there is nothing to persist.
		if dsCtx.Abstract == "" && dsCtx.Overview == "" {
			telemetry.Warn(ctx, "knowledgeproc: dataset rollup generation failed",
				otellog.String("dataset", datasetID),
				otellog.String("error", genErr.Error()))
			return
		}
		telemetry.Warn(ctx, "knowledgeproc: dataset rollup generation partially succeeded",
			otellog.String("dataset", datasetID),
			otellog.String("error", genErr.Error()))
	}

	w.deps.FSStore.SetDatasetAbstract(datasetID, dsCtx.Abstract)
	w.deps.FSStore.SetDatasetOverview(datasetID, dsCtx.Overview)
	if err := w.deps.FSStore.WriteDatasetFile(ctx, datasetID, ".abstract.md", dsCtx.Abstract); err != nil {
		telemetry.Warn(ctx, "knowledgeproc: write dataset abstract sidecar failed",
			otellog.String("dataset", datasetID),
			otellog.String("error", err.Error()))
	}
	if err := w.deps.FSStore.WriteDatasetFile(ctx, datasetID, ".overview.md", dsCtx.Overview); err != nil {
		telemetry.Warn(ctx, "knowledgeproc: write dataset overview sidecar failed",
			otellog.String("dataset", datasetID),
			otellog.String("error", err.Error()))
	}
	if w.deps.CachedStore != nil {
		w.deps.CachedStore.EvictDataset(datasetID)
	}
	if err := w.deps.AppStore.UpdateDatasetAbstract(ctx, datasetID, dsCtx.Abstract); err != nil {
		telemetry.Warn(ctx, "knowledgeproc: persist dataset abstract failed",
			otellog.String("dataset", datasetID),
			otellog.String("error", err.Error()))
	}
}

// persistDocContext writes generated L0/L1 to the FSStore (in-memory +
// sidecars) and evicts any cached entries. Returns the first error
// encountered.
func (w *Worker) persistDocContext(ctx context.Context, j job, c knowledge.DocumentContext) error {
	w.deps.FSStore.SetDocAbstract(j.datasetID, j.docName, c.Abstract)
	w.deps.FSStore.SetDocOverview(j.datasetID, j.docName, c.Overview)

	if err := w.deps.FSStore.WriteSidecar(ctx, j.datasetID, j.docName, ".abstract", c.Abstract); err != nil {
		return err
	}
	if err := w.deps.FSStore.WriteSidecar(ctx, j.datasetID, j.docName, ".overview", c.Overview); err != nil {
		return err
	}

	if w.deps.CachedStore != nil {
		w.deps.CachedStore.EvictDataset(j.datasetID)
	}
	return nil
}

// markFailed records a terminal failure for a document. The partially
// generated context (typically just an Abstract when overview failed)
// is preserved so the next reprocess does not start from scratch.
func (w *Worker) markFailed(ctx context.Context, j job, cause error, partial knowledge.DocumentContext) {
	telemetry.Warn(ctx, "knowledgeproc: document context generation failed",
		otellog.String("dataset", j.datasetID),
		otellog.String("doc", j.docID),
		otellog.String("error", cause.Error()))

	failed := model.ProcessingFailed
	patch := model.DocumentStatsPatch{
		ProcessingStatus: &failed,
	}
	if partial.Abstract != "" {
		patch.L0Abstract = strPtr(partial.Abstract)
	}
	if partial.Overview != "" {
		patch.L1Overview = strPtr(partial.Overview)
	}
	if err := w.deps.AppStore.UpdateDocumentStats(ctx, j.datasetID, j.docID, patch); err != nil {
		telemetry.Warn(ctx, "knowledgeproc: persist failed status failed",
			otellog.String("dataset", j.datasetID),
			otellog.String("doc", j.docID),
			otellog.String("error", err.Error()))
	}
}

// markEnqueueFailed flips a document straight to failed when the
// caller's context is cancelled mid-enqueue. Uses Background so the
// row state is recorded even when the request is gone.
func (w *Worker) markEnqueueFailed(ctx context.Context, datasetID, docID string, cause error) {
	failed := model.ProcessingFailed
	if err := w.deps.AppStore.UpdateDocumentStats(ctx, datasetID, docID, model.DocumentStatsPatch{
		ProcessingStatus: &failed,
	}); err != nil {
		telemetry.Warn(ctx, "knowledgeproc: persist enqueue-failed status failed",
			otellog.String("dataset", datasetID),
			otellog.String("doc", docID),
			otellog.String("cause", cause.Error()),
			otellog.String("error", err.Error()))
	}
}

func strPtr(s string) *string { return &s }
