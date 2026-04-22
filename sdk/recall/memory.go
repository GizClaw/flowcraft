package recall

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/journal"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

// Memory is the long-term-memory facade.
//
// All write paths are scope-validated; all read paths apply the
// scope-derived namespace + agent/expiry filter. Implementations are
// safe for concurrent use.
type Memory interface {
	Save(ctx context.Context, scope Scope, msgs []llm.Message) (SaveResult, error)
	SaveAsync(ctx context.Context, scope Scope, msgs []llm.Message) (JobID, error)
	JobStatus(ctx context.Context, id JobID) (JobStatus, error)
	AwaitJob(ctx context.Context, id JobID, timeout time.Duration) (JobStatus, error)

	AddRaw(ctx context.Context, scope Scope, e Entry) (string, error)
	Recall(ctx context.Context, scope Scope, req RecallRequest) ([]RecallHit, error)

	History(ctx context.Context, scope Scope, entryID string) ([]journal.Event, error)
	Rollback(ctx context.Context, scope Scope, entryID string, before time.Time) error
	Forget(ctx context.Context, scope Scope, entryID string, reason string) error

	Close() error
}

// config is the resolved configuration of a Memory instance, populated
// by the [Option] functions passed to [New]. It is package-private on
// purpose: callers compose behaviour exclusively through Option, which
// makes the surface backwards-compatible across additions.
type config struct {
	embedder embedding.Embedder
	pipe     *pipeline.Pipeline

	mode       ExtractMode
	llm        llm.LLM
	extractor  Extractor
	includeAst bool
	maxFacts   int
	confMin    float64

	saveWithCtx       bool
	saveCtxTopK       int
	saveCtxThreshold  float64

	md5Dedup           bool
	softMerge          bool
	softMergeCosineMin float64
	softMergeTopK      int

	jobQueue       JobQueue
	asyncWorkers   int
	jobMaxAttempts int
	jobBackoffBase time.Duration
	jobBackoffMax  time.Duration

	requireUserID bool
	allowGlobal   bool

	ttlPolicy       TTLPolicy
	sweeperEnabled  bool
	sweeperInterval time.Duration
	sweeperBatchMax int

	now    func() time.Time
	logger func(string, ...any)

	journal journal.Journal
}

// Option mutates a Memory configuration. All knobs are optional; the
// zero-value Memory ([New(idx)]) wires sensible defaults: in-memory job
// queue, additive extractor, MD5 dedup ON, soft-merge ON, TTL sweeper
// OFF.
type Option func(*config)

// WithEmbedder enables vector lanes for save (entry embedding) and
// recall (query embedding). Without an embedder, the pipeline runs
// BM25-only.
func WithEmbedder(e embedding.Embedder) Option { return func(c *config) { c.embedder = e } }

// WithPipeline overrides the default [pipeline.LTM]. Use this to plug
// in a custom rerank or score-decay strategy.
func WithPipeline(p *pipeline.Pipeline) Option { return func(c *config) { c.pipe = p } }

// WithLLM injects an LLM for the default additive extractor. When
// omitted, the extractor falls back to a heuristic (assistant-included)
// path that does not require model calls.
func WithLLM(l llm.LLM) Option { return func(c *config) { c.llm = l } }

// WithExtractor replaces the default extractor entirely.
func WithExtractor(e Extractor) Option { return func(c *config) { c.extractor = e } }

// WithExtractMode picks between additive and replace semantics.
// Defaults to [ModeAdditive].
func WithExtractMode(m ExtractMode) Option { return func(c *config) { c.mode = m } }

// WithIncludeAssistant tells the heuristic extractor to mine assistant
// turns alongside user turns. Has no effect when an LLM extractor is
// configured.
func WithIncludeAssistant(b bool) Option { return func(c *config) { c.includeAst = b } }

// WithMaxFactsPerCall caps the number of facts produced per Save.
func WithMaxFactsPerCall(n int) Option { return func(c *config) { c.maxFacts = n } }

// WithConfidenceMin drops extracted facts whose confidence falls below
// the threshold (range [0, 1]).
func WithConfidenceMin(f float64) Option { return func(c *config) { c.confMin = f } }

// WithSaveContext runs a top-K Recall before extraction and feeds
// snippets to the extractor as ExistingFacts. Costs one extra Recall
// per Save; turn on when extractor quality matters more than latency.
// topK <= 0 falls back to 10; threshold filters by score (0 disables).
func WithSaveContext(topK int, threshold float64) Option {
	return func(c *config) {
		c.saveWithCtx = true
		c.saveCtxTopK = topK
		c.saveCtxThreshold = threshold
	}
}

// WithoutMD5Dedup disables the per-fact md5(scope.UserID|content) dedup
// probe (default ON). Disable only if you actively want duplicate
// upserts across re-extractions.
func WithoutMD5Dedup() Option { return func(c *config) { c.md5Dedup = false } }

// WithoutSoftMerge disables soft-merging of near-duplicate older
// entries (default ON). Soft-merge marks neighbours with metadata
// `superseded_by=<new_id>`; pair with [pipeline.SupersededDecay] for
// retrieval-time damping.
func WithoutSoftMerge() Option { return func(c *config) { c.softMerge = false } }

// WithSoftMergeThreshold tunes the cosine threshold (default 0.92) and
// neighbour-fanout (default 3) for soft-merge. Values <= 0 keep the
// default.
func WithSoftMergeThreshold(cosineMin float64, topK int) Option {
	return func(c *config) {
		if cosineMin > 0 {
			c.softMergeCosineMin = cosineMin
		}
		if topK > 0 {
			c.softMergeTopK = topK
		}
	}
}

// WithJobQueue plugs in a durable [JobQueue] for SaveAsync. Defaults to
// an in-memory queue suitable for tests; production deployments should
// use [sdkx/recall/jobqueue/sqlite] or similar.
func WithJobQueue(q JobQueue) Option { return func(c *config) { c.jobQueue = q } }

// WithAsyncWorkers sets the number of background workers draining the
// JobQueue. Default 2.
func WithAsyncWorkers(n int) Option { return func(c *config) { c.asyncWorkers = n } }

// WithJobRetry configures retry behaviour for async jobs. maxAttempts
// <= 0 keeps default 5; either backoff <= 0 keeps the corresponding
// default (1s base, 5m cap).
func WithJobRetry(maxAttempts int, backoffBase, backoffMax time.Duration) Option {
	return func(c *config) {
		if maxAttempts > 0 {
			c.jobMaxAttempts = maxAttempts
		}
		if backoffBase > 0 {
			c.jobBackoffBase = backoffBase
		}
		if backoffMax > 0 {
			c.jobBackoffMax = backoffMax
		}
	}
}

// WithRequireUserID rejects any write/recall whose scope is missing
// UserID, unless paired with [WithAllowGlobal]. Use this to enforce
// per-user isolation in multi-tenant deployments.
func WithRequireUserID() Option { return func(c *config) { c.requireUserID = true } }

// WithAllowGlobal lets RequireUserID-enabled instances still serve
// runtime-global rows (UserID == ""). Has no effect without
// [WithRequireUserID].
func WithAllowGlobal() Option { return func(c *config) { c.allowGlobal = true } }

// WithTTLPolicy enables expiry on entries. The policy returns a
// duration per entry; when expired entries are recalled they are
// filtered unless the caller passes RecallRequest.WithStale = true.
func WithTTLPolicy(p TTLPolicy) Option { return func(c *config) { c.ttlPolicy = p } }

// WithSweeper enables a background goroutine that hard-deletes expired
// rows. interval <= 0 keeps default 1h; batchMax <= 0 keeps default 500.
// Requires [WithTTLPolicy] to take effect.
func WithSweeper(interval time.Duration, batchMax int) Option {
	return func(c *config) {
		c.sweeperEnabled = true
		if interval > 0 {
			c.sweeperInterval = interval
		}
		if batchMax > 0 {
			c.sweeperBatchMax = batchMax
		}
	}
}

// WithClock injects a time source (mainly for tests).
func WithClock(now func() time.Time) Option { return func(c *config) { c.now = now } }

// WithLogger sets a structured-log sink for internal warnings (e.g.
// background-job retries). nil disables logging (default).
func WithLogger(fn func(string, ...any)) Option { return func(c *config) { c.logger = fn } }

// WithJournal records every mutation for History/Rollback; required by
// the audit-trail APIs on [Memory].
func WithJournal(j journal.Journal) Option { return func(c *config) { c.journal = j } }

// lt is the canonical Memory implementation.
type lt struct {
	cfg       config
	idx       retrieval.Index
	pipe      *pipeline.Pipeline
	stopCh    chan struct{}
	wgWorkers sync.WaitGroup
}

// New constructs a Memory backed by idx. Caller must Close() on
// shutdown. idx is a positional parameter because it is the only
// non-replaceable dependency of the package.
func New(idx retrieval.Index, opts ...Option) (Memory, error) {
	if idx == nil {
		return nil, errors.New("recall: idx is required")
	}
	cfg := config{
		mode:               ModeAdditive,
		md5Dedup:           true,
		softMerge:          true,
		softMergeCosineMin: 0.92,
		softMergeTopK:      3,
		saveCtxTopK:        10,
		asyncWorkers:       2,
		jobMaxAttempts:     5,
		jobBackoffBase:     time.Second,
		jobBackoffMax:      5 * time.Minute,
		sweeperInterval:    time.Hour,
		sweeperBatchMax:    500,
		now:                time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.extractor == nil {
		cfg.extractor = &AdditiveExtractor{
			LLM:              cfg.llm,
			IncludeAssistant: cfg.includeAst || cfg.llm == nil,
			MaxFacts:         cfg.maxFacts,
			ConfidenceMin:    cfg.confMin,
		}
	}
	if cfg.jobQueue == nil {
		cfg.jobQueue = NewMemoryJobQueue()
	}
	wrapped := idx
	if cfg.journal != nil {
		wrapped = journal.Wrap(idx, cfg.journal)
	}
	pipe := cfg.pipe
	if pipe == nil {
		pipe = pipeline.LTM(cfg.embedder)
	}
	m := &lt{
		cfg:    cfg,
		idx:    wrapped,
		pipe:   pipe,
		stopCh: make(chan struct{}),
	}
	for i := 0; i < cfg.asyncWorkers; i++ {
		m.wgWorkers.Add(1)
		go m.worker()
	}
	if cfg.sweeperEnabled && cfg.ttlPolicy != nil {
		m.wgWorkers.Add(1)
		go m.sweeperLoop()
	}
	return m, nil
}

// JobStatus implements Memory.
func (m *lt) JobStatus(ctx context.Context, id JobID) (JobStatus, error) {
	rec, err := m.cfg.jobQueue.Get(ctx, id)
	if err != nil {
		return JobStatus{}, err
	}
	return statusFromRecord(rec), nil
}

// AwaitJob polls JobQueue until terminal state or timeout.
func (m *lt) AwaitJob(ctx context.Context, id JobID, timeout time.Duration) (JobStatus, error) {
	deadline := m.cfg.now().Add(timeout)
	for {
		s, err := m.JobStatus(ctx, id)
		if err != nil {
			return JobStatus{}, err
		}
		switch s.State {
		case JobSucceeded, JobFailed, JobDead:
			return s, nil
		}
		if !m.cfg.now().Before(deadline) {
			return s, ErrAwaitTimeout
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close stops workers and flushes the queue.
func (m *lt) Close() error {
	close(m.stopCh)
	m.wgWorkers.Wait()
	_ = m.cfg.jobQueue.Close()
	return m.idx.Close()
}

func (m *lt) log(format string, args ...any) {
	if m.cfg.logger != nil {
		m.cfg.logger(format, args...)
	}
}
