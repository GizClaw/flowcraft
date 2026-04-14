package knowledge

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

// Metrics for knowledge processing.
var (
	knowledgeQueueDrops, _ = telemetry.Meter().Int64Counter("knowledge_queue_drops_total",
		metric.WithDescription("Total number of dropped knowledge processing tasks due to full queue"))
)

const abstractPrompt = `Summarize the following document in ONE sentence (max 100 tokens).
Focus on: what it is, what it covers, who it's for.

Document:
%s

One-sentence summary:`

const overviewPrompt = `Create a structured overview of this document (max 1000 tokens).
Include:
- Key topics covered
- Important concepts
- Navigation hints (which sections cover what)

Document:
%s

Overview:`

const datasetOverviewPrompt = `Create an overview for this document collection based on the following summaries.
Include: what the collection covers, key documents, how they relate.

Document summaries:
%s

Collection overview:`

type taskType int

const (
	taskDocProcess taskType = iota
	taskDatasetRollup
)

const defaultMaxRetries = 3

type processingTask struct {
	datasetID string
	docName   string
	content   string
	taskType  taskType
	retries   int
}

// ProcessorOption configures a SemanticProcessor.
type ProcessorOption func(*SemanticProcessor)

// WithWorkers sets the number of concurrent workers.
func WithWorkers(n int) ProcessorOption {
	return func(p *SemanticProcessor) {
		if n > 0 {
			p.workers = n
		}
	}
}

// WithMaxRetries sets how many times a failed task is re-enqueued.
func WithMaxRetries(n int) ProcessorOption {
	return func(p *SemanticProcessor) {
		if n >= 0 {
			p.maxRetries = n
		}
	}
}

// WithOnEvict sets a callback invoked after semantic data for a dataset is updated.
// Typically used to evict the CachedStore cache.
func WithOnEvict(fn func(datasetID string)) ProcessorOption {
	return func(p *SemanticProcessor) { p.onEvict = fn }
}

// SemanticProcessor asynchronously generates L0/L1 summaries for documents
// and datasets using an LLM. Thread-safe. Failed tasks are retried up to
// maxRetries times with exponential back-off before being dropped.
type SemanticProcessor struct {
	llm        llm.LLM
	store      *FSStore
	queue      chan processingTask
	wg         sync.WaitGroup
	workers    int
	maxRetries int
	onEvict    func(datasetID string)
	ctx        context.Context
	cancel     context.CancelFunc

	mu      sync.Mutex
	cond    *sync.Cond
	pending int
}

// NewSemanticProcessor creates a processor. Call Start() to begin processing.
func NewSemanticProcessor(l llm.LLM, store *FSStore, opts ...ProcessorOption) *SemanticProcessor {
	p := &SemanticProcessor{
		llm:        l,
		store:      store,
		queue:      make(chan processingTask, 256),
		workers:    3,
		maxRetries: defaultMaxRetries,
	}
	for _, opt := range opts {
		opt(p)
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Start begins processing tasks with the configured number of workers.
func (p *SemanticProcessor) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	p.ctx = ctx
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.worker(ctx)
	}
}

// Stop waits for in-flight tasks and shuts down workers.
func (p *SemanticProcessor) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// Enqueue adds a task to the processing queue.
// Returns error if the queue is full after accounting for retries.
func (p *SemanticProcessor) Enqueue(task processingTask) error {
	p.mu.Lock()
	p.pending++
	p.mu.Unlock()

	select {
	case p.queue <- task:
		return nil
	default:
		knowledgeQueueDrops.Add(p.ctx, 1)
		telemetry.Warn(p.ctx, "knowledge: semantic processor queue full, dropping task",
			otellog.String("dataset", task.datasetID),
			otellog.String("doc", task.docName))
		p.mu.Lock()
		p.pending--
		p.cond.Broadcast()
		p.mu.Unlock()
		return errdefs.Internalf("knowledge: semantic processor queue full")
	}
}

// retryTask re-enqueues a failed task with incremented retry count and
// exponential back-off. Returns false if max retries exceeded.
func (p *SemanticProcessor) retryTask(ctx context.Context, task processingTask) bool {
	if task.retries >= p.maxRetries {
		telemetry.Warn(ctx, "knowledge: task exceeded max retries, dropping",
			otellog.String("dataset", task.datasetID),
			otellog.String("doc", task.docName),
			otellog.Int("retries", task.retries))
		return false
	}
	task.retries++
	delay := time.Duration(1<<uint(task.retries-1)) * time.Second
	go func() {
		select {
		case <-time.After(delay):
			if err := p.Enqueue(task); err != nil {
				telemetry.Warn(ctx, "knowledge: retry enqueue failed",
					otellog.String("dataset", task.datasetID),
					otellog.String("doc", task.docName),
					otellog.String("error", err.Error()))
			}
		case <-ctx.Done():
		}
	}()
	return true
}

// WaitProcessed blocks until all pending tasks have been processed.
func (p *SemanticProcessor) WaitProcessed(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.mu.Lock()
		for p.pending > 0 {
			p.cond.Wait()
		}
		p.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		p.cond.Broadcast()
		return ctx.Err()
	}
}

func (p *SemanticProcessor) worker(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case task, ok := <-p.queue:
			if !ok {
				return
			}
			p.processTask(ctx, task)
			p.mu.Lock()
			p.pending--
			p.cond.Broadcast()
			p.mu.Unlock()
		}
	}
}

func (p *SemanticProcessor) processTask(ctx context.Context, task processingTask) {
	switch task.taskType {
	case taskDocProcess:
		p.processDocument(ctx, task)
	case taskDatasetRollup:
		p.processDatasetRollup(ctx, task)
	}
}

func (p *SemanticProcessor) processDocument(ctx context.Context, task processingTask) {
	// Generate L0 abstract
	abstract, err := p.generate(ctx, fmt.Sprintf(abstractPrompt, truncateForPrompt(task.content, 8000)))
	if err != nil {
		telemetry.Warn(ctx, "knowledge: L0 generation failed",
			otellog.String("doc", task.docName), otellog.String("error", err.Error()),
			otellog.Int("retry", task.retries))
		p.retryTask(ctx, task)
		return
	}
	if err := p.store.WriteSidecar(ctx, task.datasetID, task.docName, ".abstract", abstract); err != nil {
		telemetry.Warn(ctx, "knowledge: write L0 failed",
			otellog.String("doc", task.docName), otellog.String("error", err.Error()))
	}
	p.store.SetDocAbstract(task.datasetID, task.docName, abstract)

	// Generate L1 overview
	overview, err := p.generate(ctx, fmt.Sprintf(overviewPrompt, truncateForPrompt(task.content, 8000)))
	if err != nil {
		telemetry.Warn(ctx, "knowledge: L1 generation failed",
			otellog.String("doc", task.docName), otellog.String("error", err.Error()),
			otellog.Int("retry", task.retries))
		p.retryTask(ctx, task)
		return
	}
	if err := p.store.WriteSidecar(ctx, task.datasetID, task.docName, ".overview", overview); err != nil {
		telemetry.Warn(ctx, "knowledge: write L1 failed",
			otellog.String("doc", task.docName), otellog.String("error", err.Error()))
	}
	p.store.SetDocOverview(task.datasetID, task.docName, overview)

	if p.onEvict != nil {
		p.onEvict(task.datasetID)
	}

	// Trigger dataset rollup
	_ = p.Enqueue(processingTask{
		datasetID: task.datasetID,
		taskType:  taskDatasetRollup,
	})
}

func (p *SemanticProcessor) processDatasetRollup(ctx context.Context, task processingTask) {
	p.store.mu.RLock()
	di, ok := p.store.index[task.datasetID]
	if !ok {
		p.store.mu.RUnlock()
		return
	}

	// Copy docs to snapshot to shorten lock hold time
	snapshot := make([]*Document, 0, len(di.docs))
	for _, doc := range di.docs {
		snapshot = append(snapshot, doc)
	}
	p.store.mu.RUnlock()

	var abstracts []string
	for _, doc := range snapshot {
		if doc.Abstract != "" {
			abstracts = append(abstracts, fmt.Sprintf("- %s: %s", doc.Name, doc.Abstract))
		}
	}

	if len(abstracts) == 0 {
		return
	}

	summariesText := strings.Join(abstracts, "\n")

	// Generate dataset-level L1
	dsOverview, err := p.generate(ctx, fmt.Sprintf(datasetOverviewPrompt, summariesText))
	if err != nil {
		telemetry.Warn(ctx, "knowledge: dataset L1 generation failed",
			otellog.String("dataset", task.datasetID), otellog.String("error", err.Error()),
			otellog.Int("retry", task.retries))
		p.retryTask(ctx, task)
		return
	}
	if err := p.store.WriteDatasetFile(ctx, task.datasetID, ".overview.md", dsOverview); err != nil {
		telemetry.Warn(ctx, "knowledge: write dataset L1 failed",
			otellog.String("dataset", task.datasetID), otellog.String("error", err.Error()))
	}
	p.store.SetDatasetOverview(task.datasetID, dsOverview)

	// Distill dataset-level L0 from L1
	dsAbstract, err := p.generate(ctx, fmt.Sprintf(abstractPrompt, dsOverview))
	if err != nil {
		telemetry.Warn(ctx, "knowledge: dataset L0 generation failed",
			otellog.String("dataset", task.datasetID), otellog.String("error", err.Error()))
		p.retryTask(ctx, task)
		return
	}
	if err := p.store.WriteDatasetFile(ctx, task.datasetID, ".abstract.md", dsAbstract); err != nil {
		telemetry.Warn(ctx, "knowledge: write dataset L0 failed",
			otellog.String("dataset", task.datasetID), otellog.String("error", err.Error()))
	}
	p.store.SetDatasetAbstract(task.datasetID, dsAbstract)

	if p.onEvict != nil {
		p.onEvict(task.datasetID)
	}
}

func (p *SemanticProcessor) generate(ctx context.Context, prompt string) (string, error) {
	resp, _, err := p.llm.Generate(ctx, []llm.Message{
		llm.NewTextMessage(llm.RoleUser, prompt),
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content()), nil
}

func truncateForPrompt(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "\n...(truncated)"
}
