package recall

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/journal"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

// Memory is the long-term-memory facade — the read/write contract every
// recall implementation must satisfy.
//
// All write paths are scope-validated; all read paths apply the
// scope-derived namespace + agent/expiry filter. Implementations are
// safe for concurrent use.
//
// Audit (History / Rollback) and async job control (JobStatus /
// AwaitJob) are exposed through the optional [Auditable] and
// [JobController] sub-interfaces, not here, so alternative Memory
// implementations (e.g. an in-memory test double) do not have to
// implement them. Callers that need those capabilities type-assert:
//
//	if jc, ok := mem.(recall.JobController); ok { … }
type Memory interface {
	// Save extracts facts from msgs and writes them synchronously.
	Save(ctx context.Context, scope Scope, msgs []llm.Message) (SaveResult, error)

	// SaveAsync enqueues extraction on the configured JobQueue and
	// returns immediately.
	SaveAsync(ctx context.Context, scope Scope, msgs []llm.Message) (JobID, error)

	// Add inserts one pre-built Entry verbatim. Returns the assigned
	// entry ID (content-addressable when e.ID is empty).
	Add(ctx context.Context, scope Scope, e Entry) (string, error)

	// Recall runs the configured retrieval pipeline against the
	// scope-derived namespace.
	Recall(ctx context.Context, scope Scope, req Request) ([]Hit, error)

	// Forget hard-deletes one entry; journal (when configured)
	// captures the reason.
	Forget(ctx context.Context, scope Scope, entryID string, reason string) error

	// Close stops async workers and the TTL sweeper; safe to call more
	// than once.
	Close() error
}

// Auditable is implemented by [Memory] flavours that persist a
// [journal.Journal]. Callers must type-assert at construction time:
//
//	aud, ok := mem.(recall.Auditable)
type Auditable interface {
	History(ctx context.Context, scope Scope, entryID string) ([]journal.Event, error)
	Rollback(ctx context.Context, scope Scope, entryID string, before time.Time) error
}

// JobController is implemented by [Memory] flavours that back
// SaveAsync with an inspectable [JobQueue]. Callers type-assert at
// construction time.
type JobController interface {
	JobStatus(ctx context.Context, id JobID) (JobStatus, error)
	AwaitJob(ctx context.Context, id JobID, timeout time.Duration) (JobStatus, error)
}

// RecallExplainer is implemented by [Memory] flavours whose underlying
// retrieval pipeline can produce a structured [retrieval.SearchExecution]
// (lanes, stages, …) alongside the ranked hits.
//
// RecallExplain has the same scope/validation contract as [Memory.Recall];
// callers populate Request.Debug to opt in. The returned execution is nil
// when Debug is the zero value or when no stage produced one.
//
// Type-assert to use it:
//
//	if rx, ok := mem.(recall.RecallExplainer); ok { … }
type RecallExplainer interface {
	RecallExplain(ctx context.Context, scope Scope, req Request) ([]Hit, *retrieval.SearchExecution, error)
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

	saveWithCtx      bool
	saveCtxTopK      int
	saveCtxThreshold float64

	md5Dedup           bool
	softMerge          bool
	slotMerge          bool
	softMergeCosineMin float64
	softMergeTopK      int

	updateResolver UpdateResolver
	resolverTopK   int

	predicateAliases map[string]string
	subjectAliases   map[string]string

	jobQueue       JobQueue
	asyncWorkers   int
	jobMaxAttempts int
	jobBackoffBase time.Duration
	jobBackoffMax  time.Duration
	jobTimeout     time.Duration

	requireUserID bool
	allowGlobal   bool

	ttlPolicy       TTLPolicy
	sweeperEnabled  bool
	sweeperInterval time.Duration
	sweeperBatchMax int
	nsRegistry      NamespaceRegistry

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

// WithoutSoftMerge disables the VECTOR + entity-set supersede channel
// (default ON). The vector channel marks near-duplicate older
// neighbours with metadata `superseded_by=<new_id>` based on cosine
// similarity and entity overlap; pair with [pipeline.SupersededDecay]
// for retrieval-time damping.
//
// This option does NOT affect the deterministic SLOT supersede
// channel — facts that carry both Subject and Predicate continue to
// supersede same-slot neighbours regardless. Use [WithoutSlotChannel]
// to disable the slot channel as well. The two channels are
// orthogonal so callers can keep the deterministic path while
// silencing the noisier vector path (e.g. when entity extraction is
// unreliable on their corpus).
func WithoutSoftMerge() Option { return func(c *config) { c.softMerge = false } }

// WithoutSlotChannel disables the deterministic (subject, predicate)
// supersede channel that runs whenever an extractor populates both
// slot fields (default ON). Use this when you want to keep the
// vector soft-merge path but disable the slot path — for example
// when migrating an existing namespace whose historical entries were
// written without slot metadata, and you want the read-side
// SupersededDecay to depend exclusively on vector-channel decisions
// for a controlled rollout window.
func WithoutSlotChannel() Option { return func(c *config) { c.slotMerge = false } }

// WithUpdateResolver installs an opt-in LLM-driven resolver that is
// consulted on Save for facts the slot supersede channel cannot handle
// (i.e. ExtractedFact.Subject or Predicate is empty). The resolver
// receives the new fact plus its top-K nearest existing memories and
// returns a list of [ResolveAction] (ADD / UPDATE / DELETE / NOOP).
//
// FlowCraft applies UPDATE and DELETE non-destructively: targeted
// entries gain superseded_by metadata (DELETE additionally sets
// tombstone=true), preserving Auditable.History and Rollback. ADD and
// NOOP require no action since the new entry is always written.
//
// topK <= 0 falls back to 5. Passing a nil resolver disables the path
// (the default).
//
// Ordering note: the resolver runs AFTER the slot and vector
// supersede channels. Candidates whose older versions were already
// tagged by those channels will appear with damped scores (or be
// missing from the top-K entirely) thanks to SupersededDecay; this
// is intentional — the resolver should not re-decide what the
// deterministic channels already handled. Operators reading the
// resolver_actions_total counter should expect candidate counts to
// shrink when slot vocabulary coverage improves.
func WithUpdateResolver(r UpdateResolver, topK int) Option {
	return func(c *config) {
		c.updateResolver = r
		if topK > 0 {
			c.resolverTopK = topK
		} else {
			c.resolverTopK = 5
		}
	}
}

// WithPredicateAlias merges additional predicate aliases on top of the
// built-in [PredicateAliases] table. Use this to teach the slot
// supersede channel about namespace-specific synonyms (e.g. medical
// SaaS adding "primary_care_physician" → "doctor"). Keys MUST be
// lowercase + trimmed; values SHOULD match a canonical predicate.
//
// Per-instance overrides win over the global table so callers can
// remap a built-in entry if needed without forking the source.
func WithPredicateAlias(aliases map[string]string) Option {
	return func(c *config) {
		if len(aliases) == 0 {
			return
		}
		if c.predicateAliases == nil {
			c.predicateAliases = make(map[string]string, len(aliases))
		}
		maps.Copy(c.predicateAliases, aliases)
	}
}

// WithSubjectAlias mirrors [WithPredicateAlias] for ExtractedFact.Subject.
// Composite subjects (those containing ':' or '.') are passed through
// without aliasing so per-instance subjects like "pet:Lucky" remain
// distinguishable.
func WithSubjectAlias(aliases map[string]string) Option {
	return func(c *config) {
		if len(aliases) == 0 {
			return
		}
		if c.subjectAliases == nil {
			c.subjectAliases = make(map[string]string, len(aliases))
		}
		maps.Copy(c.subjectAliases, aliases)
	}
}

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

// WithJobTimeout caps the per-job execution budget. A worker that
// exceeds it sees its context canceled, the extractor / index call
// returns ctx.Err(), and the job is rescheduled (or sent to dead via
// the normal failOrRetry path). Defaults to 5 minutes; pass 0 to keep
// the default.
//
// This bound also guarantees [Memory.Close] never blocks longer than
// timeout + the time needed to drain currently-leased jobs, because
// Close cancels the worker context which is propagated into Extract
// and the index Upsert.
func WithJobTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.jobTimeout = d
		}
	}
}

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
// filtered unless the caller passes Request.WithStale = true.
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

// WithNamespaceRegistry overrides the registry used to track namespaces for
// background sweeps. Defaults to an in-memory implementation.
func WithNamespaceRegistry(r NamespaceRegistry) Option {
	return func(c *config) {
		if r != nil {
			c.nsRegistry = r
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

// lt is the canonical Memory implementation. It satisfies the core
// [Memory] contract plus the optional [Auditable] and [JobController]
// sub-interfaces; callers that need the audit or job APIs obtain them
// via type assertion on the Memory returned by [New].
//
// workerCtx / workerCancel propagate Close() into in-flight jobs: the
// worker derives a per-job context from workerCtx with the configured
// timeout, so cancelling workerCtx (Close) bounds Close()'s wait by the
// extractor / index call's responsiveness to ctx cancellation.
type lt struct {
	cfg       config
	idx       retrieval.Index
	pipe      *pipeline.Pipeline
	stopCh    chan struct{}
	wgWorkers sync.WaitGroup

	workerCtx    context.Context
	workerCancel context.CancelFunc
}

var (
	_ Memory        = (*lt)(nil)
	_ Auditable     = (*lt)(nil)
	_ JobController = (*lt)(nil)
)

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
		slotMerge:          true,
		softMergeCosineMin: 0.92,
		softMergeTopK:      3,
		saveCtxTopK:        10,
		asyncWorkers:       2,
		jobMaxAttempts:     5,
		jobBackoffBase:     time.Second,
		jobBackoffMax:      5 * time.Minute,
		jobTimeout:         5 * time.Minute,
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
	if cfg.sweeperEnabled && cfg.nsRegistry == nil {
		cfg.nsRegistry = NewMemoryNamespaceRegistry()
	}
	wrapped := idx
	if cfg.journal != nil {
		wrapped = journal.Wrap(idx, cfg.journal)
	}
	pipe := cfg.pipe
	if pipe == nil {
		pipe = pipeline.LTM(cfg.embedder)
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	m := &lt{
		cfg:          cfg,
		idx:          wrapped,
		pipe:         pipe,
		stopCh:       make(chan struct{}),
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
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
			return s, errdefs.FromContext(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close stops workers and flushes the queue.
//
// Close is bounded: cancelling workerCtx propagates into the per-job
// context derived in handleJob, so an extractor or index call that
// honours ctx.Done() will return promptly. Close still Wait()s on the
// worker goroutines themselves, so a non-responsive backend can still
// delay shutdown by up to its own internal timeout — but it can no
// longer block forever on a stuck LLM call (the worst case is bounded
// by [WithJobTimeout], default 5 minutes).
//
// Idempotent: subsequent calls observe a closed stopCh and a drained
// WaitGroup and return immediately. workerCancel is also idempotent
// per the context.CancelFunc contract.
func (m *lt) Close() error {
	select {
	case <-m.stopCh:
		// already closed
	default:
		close(m.stopCh)
	}
	m.workerCancel()
	m.wgWorkers.Wait()
	var nsErr error
	if m.cfg.nsRegistry != nil {
		nsErr = m.cfg.nsRegistry.Close()
	}
	return errors.Join(
		nsErr,
		m.cfg.jobQueue.Close(),
		m.idx.Close(),
	)
}

func (m *lt) log(format string, args ...any) {
	if m.cfg.logger != nil {
		m.cfg.logger(format, args...)
	}
}

func (m *lt) rememberNamespace(ctx context.Context, ns string) {
	if ns == "" || m.cfg.nsRegistry == nil {
		return
	}
	if err := m.cfg.nsRegistry.Remember(ctx, ns); err != nil && ctx.Err() == nil {
		m.log("recall: remember namespace %q: %v", ns, err)
	}
}
