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
// Implementations are safe for concurrent use. All write paths are
// scope-validated; all read paths apply scope-derived namespace + filter.
type Memory interface {
	Save(ctx context.Context, scope MemoryScope, msgs []llm.Message) (SaveResult, error)
	SaveAsync(ctx context.Context, scope MemoryScope, msgs []llm.Message) (JobID, error)
	JobStatus(ctx context.Context, id JobID) (JobStatus, error)
	AwaitJob(ctx context.Context, id JobID, timeout time.Duration) (JobStatus, error)

	AddRaw(ctx context.Context, scope MemoryScope, e MemoryEntry) (string, error)
	Recall(ctx context.Context, scope MemoryScope, req RecallRequest) ([]RecallHit, error)

	History(ctx context.Context, scope MemoryScope, entryID string) ([]journal.Event, error)
	Rollback(ctx context.Context, scope MemoryScope, entryID string, before time.Time) error
	Forget(ctx context.Context, scope MemoryScope, entryID string, reason string) error

	Close() error
}

// Config configures a new Memory instance.
type Config struct {
	Index    retrieval.Index
	Embedder embedding.Embedder
	Pipeline *pipeline.Pipeline

	Mode             ExtractMode
	LLM              llm.LLM
	Extractor        Extractor
	IncludeAssistant bool
	MaxFactsPerCall  int
	ConfidenceMin    float64

	// SaveWithContext, when true, runs a top-K Recall before extraction and
	// hands the snippets to the Extractor as ExtractOptions.ExistingFacts so
	// the LLM can avoid restating known facts.
	SaveWithContext      bool
	SaveContextTopK      int // default 10 when SaveWithContext is true
	SaveContextThreshold float64

	// MD5Dedup, when true, skips facts whose md5(scope.UserID|content) already
	// exists in the namespace. Default true.
	MD5Dedup bool

	// SoftMerge, when true, marks near-duplicate older entries with
	// metadata.superseded_by = <new_id>. Pair with pipeline.SupersededDecay
	// for retrieval-time damping. Default true.
	SoftMerge bool
	// SoftMergeCosineMin is the cosine threshold above which an existing entry
	// with the same entity set is considered a candidate for supersession.
	// Default 0.92.
	SoftMergeCosineMin float64
	// SoftMergeTopK controls how many neighbours are inspected per new fact.
	// Default 3.
	SoftMergeTopK int

	JobQueue       JobQueue
	AsyncWorkers   int
	JobMaxAttempts int
	JobBackoffBase time.Duration
	JobBackoffMax  time.Duration

	RequireUserID bool
	AllowGlobal   bool

	TTLPolicy       TTLPolicy
	SweeperEnabled  bool
	SweeperInterval time.Duration
	SweeperBatchMax int

	Now    func() time.Time
	Logger func(string, ...any)

	// Journal is optional. When set, mutations are recorded for History/Rollback.
	Journal journal.Journal
}

// lt is the canonical Memory implementation.
type lt struct {
	cfg       Config
	idx       retrieval.Index
	pipe      *pipeline.Pipeline
	stopCh    chan struct{}
	wgWorkers sync.WaitGroup
}

// New constructs a Memory backed by cfg.Index. Caller must Close() on shutdown.
func New(cfg Config) (Memory, error) {
	if cfg.Index == nil {
		return nil, errors.New("ltm: Config.Index is required")
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeAdditive
	}
	if cfg.Extractor == nil {
		cfg.Extractor = &AdditiveExtractor{
			LLM:              cfg.LLM,
			IncludeAssistant: cfg.IncludeAssistant || cfg.LLM == nil,
			MaxFacts:         cfg.MaxFactsPerCall,
			ConfidenceMin:    cfg.ConfidenceMin,
		}
	}
	if cfg.JobQueue == nil {
		cfg.JobQueue = NewMemoryJobQueue()
	}
	if cfg.AsyncWorkers <= 0 {
		cfg.AsyncWorkers = 2
	}
	if cfg.JobMaxAttempts <= 0 {
		cfg.JobMaxAttempts = 5
	}
	if cfg.JobBackoffBase <= 0 {
		cfg.JobBackoffBase = time.Second
	}
	if cfg.JobBackoffMax <= 0 {
		cfg.JobBackoffMax = 5 * time.Minute
	}
	if cfg.SweeperInterval <= 0 {
		cfg.SweeperInterval = time.Hour
	}
	if cfg.SweeperBatchMax <= 0 {
		cfg.SweeperBatchMax = 500
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SaveContextTopK <= 0 {
		cfg.SaveContextTopK = 10
	}
	if cfg.SoftMergeCosineMin <= 0 {
		cfg.SoftMergeCosineMin = 0.92
	}
	if cfg.SoftMergeTopK <= 0 {
		cfg.SoftMergeTopK = 3
	}
	// Defaults for boolean flags: opt-IN here so zero-value Config gets the
	// recommended behaviour. Users wanting the legacy ADD-only path can flip
	// these to false explicitly.
	if !cfg.SoftMerge && !cfg.MD5Dedup && !cfg.SaveWithContext {
		// Heuristic: if all three are unset, the caller likely accepted the
		// defaults; turn on MD5Dedup + SoftMerge but leave SaveWithContext
		// off (it requires an additional Recall call per Save).
		cfg.MD5Dedup = true
		cfg.SoftMerge = true
	}

	idx := cfg.Index
	if cfg.Journal != nil {
		idx = journal.Wrap(cfg.Index, cfg.Journal)
	}
	pipe := cfg.Pipeline
	if pipe == nil {
		pipe = pipeline.LTM(cfg.Embedder)
	}
	m := &lt{
		cfg:    cfg,
		idx:    idx,
		pipe:   pipe,
		stopCh: make(chan struct{}),
	}
	for i := 0; i < cfg.AsyncWorkers; i++ {
		m.wgWorkers.Add(1)
		go m.worker()
	}
	if cfg.SweeperEnabled && cfg.TTLPolicy != nil {
		m.wgWorkers.Add(1)
		go m.sweeperLoop()
	}
	return m, nil
}

// JobStatus implements Memory.
func (m *lt) JobStatus(ctx context.Context, id JobID) (JobStatus, error) {
	rec, err := m.cfg.JobQueue.Get(ctx, id)
	if err != nil {
		return JobStatus{}, err
	}
	return statusFromRecord(rec), nil
}

// AwaitJob polls JobQueue until terminal state or timeout.
func (m *lt) AwaitJob(ctx context.Context, id JobID, timeout time.Duration) (JobStatus, error) {
	deadline := m.cfg.Now().Add(timeout)
	for {
		s, err := m.JobStatus(ctx, id)
		if err != nil {
			return JobStatus{}, err
		}
		switch s.State {
		case JobSucceeded, JobFailed, JobDead:
			return s, nil
		}
		if !m.cfg.Now().Before(deadline) {
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
	_ = m.cfg.JobQueue.Close()
	return m.idx.Close()
}

func (m *lt) log(format string, args ...any) {
	if m.cfg.Logger != nil {
		m.cfg.Logger(format, args...)
	}
}
