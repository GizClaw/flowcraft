package recall

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/internal/syncx"
	"github.com/GizClaw/flowcraft/sdk/llm"
	recallpipe "github.com/GizClaw/flowcraft/sdk/recall_v1/pipeline"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/journal"
	basepipe "github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
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

	// Add inserts one pre-built Entry verbatim and returns the
	// assigned entry ID.
	//
	// Two ingest modes:
	//
	//   - e.ID == "" (content-addressable): the ID is derived
	//     deterministically from (scope, Content). Two Adds with
	//     the same payload collide on ID; the second call is
	//     short-circuited via the per-namespace content-hash
	//     dedup probe (the same gate that [Memory.Save] uses for
	//     fact upsert), making Add idempotent against retries.
	//     Set [WithoutMD5Dedup] to skip the probe; repeat content
	//     still collides on ID, so the Upsert overwrites in
	//     place.
	//   - e.ID != "" (caller-owned identity): the supplied ID is
	//     honoured verbatim and the dedup probe is SKIPPED so
	//     two writes with the same Content but different IDs
	//     (e.g. timestamped event replays) stay as separate
	//     rows.
	//
	// Fix landed in #155.
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

// SideStoreSyncer is implemented by [Memory] flavours that register
// one or more [Projection]s. Calling SyncSideStores runs one
// synchronous reconcile pass for the given scope, bringing every
// registered projection to the alive state of the primary index as
// of now.
//
// Use this from tests and from callers that just performed a write
// outside the eager hot path (Add, Auditable.Rollback, Memory.Forget,
// TTL sweep, resolver OpDelete) and want 0-lag entity-lane recall
// before the next background tick. Returns nil immediately when no
// projection is registered.
type SideStoreSyncer interface {
	SyncSideStores(ctx context.Context, scope Scope) error
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
	pipe     *basepipe.Pipeline

	mode       ExtractMode
	llm        llm.LLM
	extractor  Extractor
	includeAst bool
	maxFacts   int
	confMin    float64

	saveWithCtx      bool
	saveCtxTopK      int
	saveCtxThreshold float64

	// historyStore is the per-conversation message log consulted
	// before extraction so the LLM can resolve pronoun / short-
	// reference / chronology references established in PRIOR Save
	// batches but absent from the current batch. See
	// WithHistoryStore + WithRecentTurns. recentMsgsK is the number
	// of messages the Save path injects into the extractor; 0
	// disables the feature even if a store is configured.
	historyStore history.Store
	recentMsgsK  int

	// entityStore is the inverted-index accelerator written
	// alongside every entry upsert and consulted at recall time by
	// the EntityLinkLookup pipeline stage. Nil disables the feature
	// — Save then skips its Link call and Recall sees no
	// candidate IDs from this lane. See [EntityStore] and
	// [WithEntityStore] for the lifecycle contract.
	//
	// entityStoreLinkedCap mirrors IndexEntityStoreOptions.LinkedCap
	// and is captured here only so the lt.upsertFacts path can pass
	// the option through to the concrete IndexEntityStore at
	// construction time; once a store is wired its own cap wins.
	entityStore             EntityStore
	entityStoreLinkedCap    int
	entityStoreMaxLinkedCnt int
	// entityStoreMaxLinkedCntExplicit tracks whether the caller
	// passed [WithEntityStoreMaxLinkedCount] at all. Needed because
	// the field's zero value coincides with "use safe default"
	// AND with "caller wrote 0" — we only emit the construction-
	// time warning when the caller EXPLICITLY chose the disabled
	// path (n < 0), and we only override the safe default when the
	// caller deliberately passed n > 0.
	entityStoreMaxLinkedCntExplicit bool

	// queryEntityLLM, when set, swaps the pipeline's rule-based
	// query-side entity extraction for an LLM-backed extractor. The
	// rule extractor pulls only capitalized single tokens + quoted
	// runs, so multi-word noun phrases ("photography club") never
	// land in QueryEntities — making them un-joinable against the
	// LLM-extracted entity names persisted by EntityStore.Link at
	// write time. The LLM extractor closes this asymmetry. See
	// [WithQueryEntityExtractor].
	queryEntityLLM llm.LLM

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

	// reconcileInterval controls the cadence of the background
	// [Reconciler] that brings side-store projections (currently
	// the EntityStore) into eventual consistency with the primary
	// index. Zero falls back to [defaultReconcileInterval] (5 min)
	// whenever any projection is active; negative disables the
	// background tick (synchronous [Memory.SyncSideStores] still
	// works). See [WithReconcileInterval].
	reconcileInterval time.Duration

	now    func() time.Time
	logger func(string, ...any)

	journal journal.Journal

	// ltmOpts are passed verbatim into [pipeline.LTM] when the
	// auto-wired pipeline is constructed (i.e. when WithPipeline is
	// NOT used). Single source of truth for LTM tuning so the
	// auto-wire path can append its own options
	// (entity-link lane et al.) without the caller having to
	// remember to thread them too. See [WithLTMOption] for the
	// design rationale.
	ltmOpts       []recallpipe.LTMOption
	legacyLTMOpts []basepipe.LTMOption
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

// WithPipeline overrides the default [pipeline.LTM]. Use this to
// plug in a fully custom topology that is NOT LTM-shaped. When
// supplied, [WithLTMOption] and feature flags that auto-wire LTM
// (notably [WithEntityStore]) are SKIPPED — the caller has taken
// full responsibility for the read path.
//
// Prefer [WithLTMOption] for the common case of "I want LTM with a
// few knobs adjusted"; that path keeps feature auto-wiring intact.
func WithPipeline(p *basepipe.Pipeline) Option { return func(c *config) { c.pipe = p } }

// WithLTMOption appends [pipeline.LTMOption]s to the auto-wired
// [pipeline.LTM] pipeline. Stack multiple calls — they accumulate
// in declaration order, and feature flags ([WithEntityStore], …)
// then append THEIR options on top so the final pipeline reflects
// both the caller's tuning AND every wired feature.
//
// Design rationale: prior to this option, callers who wanted to
// tune LTM had to build the pipeline themselves and pass it via
// [WithPipeline] — which bypassed every feature flag's auto-wire
// path. The net effect was that turning on, say,
// [WithEntityStore] would silently activate the WRITE path (Link
// on Save) while leaving the READ path (entity-link lane) absent
// because the user's custom pipeline did not include it. Funneling
// LTM tuning through this option restores a single source of
// truth: features add to the recipe; users add to the recipe; the
// pipeline gets built once at the end.
//
// Accepts sdk/recall/pipeline.LTMOption. Deprecated
// sdk/retrieval/pipeline.LTMOption values are still accepted as a compatibility
// bridge, but that path constructs the old retrieval-level LTM recipe and will
// be removed in v0.5.0.
//
// No-op when [WithPipeline] is also set (the custom pipeline wins
// and a warning is logged).
func WithLTMOption(opts ...any) Option {
	return func(c *config) {
		for _, opt := range opts {
			switch o := opt.(type) {
			case nil:
				continue
			case basepipe.LTMOption:
				c.legacyLTMOpts = append(c.legacyLTMOpts, o)
			default:
				c.ltmOpts = append(c.ltmOpts, o)
			}
		}
	}
}

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

// WithHistoryStore wires a [history.Store] into the recall pipeline
// so every Save reads the previous K messages from the same
// conversation BEFORE extraction (injected as
// [ExtractOptions.RecentMessages]) and APPENDS the current Save's
// messages AFTER persistence. This unlocks cross-batch pronoun /
// anaphora / entity-reference resolution for ingest topologies that
// hand the recall layer many small Save batches per conversation
// (chat applications, streaming transcripts, per-turn ingestion).
//
// k <= 0 disables the injection even when store is non-nil. A nil
// store also disables the feature.
//
// The conversation key fed to the store is derived from the Save's
// Scope via [NamespaceFor]; one conversation per Scope is the
// expected mapping. Callers needing finer-grained conversation
// boundaries (e.g. several distinct chats per user) should layer
// per-conversation scopes on top of this option instead of inventing
// a parallel buffer.
func WithHistoryStore(store history.Store, k int) Option {
	return func(c *config) {
		if store == nil || k <= 0 {
			return
		}
		c.historyStore = store
		c.recentMsgsK = k
	}
}

// WithRecentTurns is a convenience shortcut that installs an
// in-process [history.InMemoryStore] and sets the per-Save read
// window to k. Use [WithHistoryStore] directly to plug in a
// persistent backend (history.FileStore, a Redis-backed
// implementation, etc.).
//
// k <= 0 disables the feature.
func WithRecentTurns(k int) Option {
	return func(c *config) {
		if k <= 0 {
			return
		}
		c.historyStore = history.NewInMemoryStore()
		c.recentMsgsK = k
	}
}

// WithEntityStore enables the entity-link inverted index. The store
// is written synchronously on Save (best-effort: failure logs but
// does not roll back the entry upsert) and consulted at Recall time
// by the EntityLinkLookup pipeline stage when the configured
// pipeline includes it (see [pipeline.WithEntityLink]).
//
// Pass linkedCap <= 0 to keep the default ([defaultEntityLinkedCap]).
// The store is constructed lazily inside [New] so callers do not
// have to thread the underlying retrieval.Index themselves: the same
// index passed to New backs both the entry rows and the sibling
// entity namespace. Backends that do not implement
// [retrieval.DocGetter] cause the feature to silently degrade to
// "not wired" — a log line is emitted at construction time and
// Save/Recall behave exactly as if the option were absent.
//
// Default OFF. Phased rollout: opt in per-deployment, observe
// retrieval-quality metrics, then promote to default once broad
// conversational-memory workloads justify the extra write/read cost.
//
// Common-noun gate: enabling the entity store also activates the
// pollution gate at [defaultEntityMaxLinkedCount] (100). Tune via
// [WithEntityStoreMaxLinkedCount]. Disabling the gate (negative
// value) is the documented audited opt-out. The gate exists because
// saturated entity rows can otherwise flood RRF with low-information
// candidates; "just turn the feature on" now lands on the safe path.
func WithEntityStore(linkedCap int) Option {
	return func(c *config) {
		// We can't construct the IndexEntityStore here because the
		// retrieval.Index isn't available until New(); record the
		// caller's intent + cap and let New() finish the wire-up.
		c.entityStore = entityStoreSentinel
		c.entityStoreLinkedCap = linkedCap
	}
}

// WithEntityStoreMaxLinkedCount tunes the common-noun pollution
// gate documented on [IndexEntityStoreOptions.MaxLinkedCount]: any
// entity row whose linked_ids count strictly exceeds n is silently
// skipped at Lookup time so the entity-link lane does not vote that
// row's (low-signal) entries past the vector / BM25 lanes' picks.
//
// Sentinel semantics:
//
//   - n > 0: that exact threshold.
//   - n == 0: leave the default in place ([defaultEntityMaxLinkedCount]
//     = 100, applied by [NewIndexEntityStore]). Use this when you
//     just want the safe production default, no opinion.
//   - n < 0: EXPLICITLY DISABLE the gate. Turning it off is
//     intentional and audited — [Memory.New] logs a one-time
//     warning at construction so the opt-out leaves a paper
//     trail. Use only when --dump-recall histograms prove your
//     corpus's MetaEntityCount distribution is gate-tolerant.
//
// Must be paired with [WithEntityStore]; sets the option on the
// IndexEntityStore that auto-constructs in [New].
func WithEntityStoreMaxLinkedCount(n int) Option {
	return func(c *config) {
		c.entityStoreMaxLinkedCnt = n
		c.entityStoreMaxLinkedCntExplicit = true
	}
}

// WithReconcileInterval sets the cadence of the background
// [Reconciler] that keeps side-store [Projection]s eventually
// consistent with the primary index.
//
//   - d > 0: the Reconciler ticks every d. Smaller d = fresher
//     projections at the cost of more namespace scans.
//   - d == 0: the recall package uses [defaultReconcileInterval]
//     (5 min) whenever any projection is registered.
//   - d  < 0: the background loop is disabled. Synchronous
//     [Memory.SyncSideStores] still works — useful in tests and
//     in deployments that drive reconciliation from their own
//     scheduler.
//
// This option has no effect when no projection is configured
// ([WithEntityStore] is the only projection source today; future
// graph / analytics views will register through the same channel).
func WithReconcileInterval(d time.Duration) Option {
	return func(c *config) { c.reconcileInterval = d }
}

// WithQueryEntityExtractor installs an LLM-backed extractor for the
// query-side `QueryEntities` slot in the retrieval pipeline state.
//
// Default (when this option is absent) is the rule-based extractor
// (see [pipeline.EntityExtract] / pipeline.ruleEntities) — capitalized
// single tokens + quoted runs only. That is sufficient for vector +
// BM25 entity boost, but it is asymmetric with the LLM-extracted,
// multi-word entity phrases the write-side extractor persists into
// the EntityStore (e.g. "photography club"). The asymmetry
// silently collapses entity-link recall to a tiny join surface in
// entity-dense conversational workloads.
//
// Wiring this option causes the auto-wired LTM pipeline to swap its
// rule-based [pipeline.EntityExtract] for an LLM-backed extractor
// (built on top of `client`) so query-side entity strings share the
// same vocabulary as the write-side EntityStore keys.
//
// Cost: one extra LLM call per recall, ~150-400 ms latency added.
// Best-effort: on any LLM / parse error, the extractor returns an
// empty slice and the recall still completes (no extra failure
// surface).
//
// nil disables the feature (rule extractor remains).
func WithQueryEntityExtractor(client llm.LLM) Option {
	return func(c *config) {
		c.queryEntityLLM = client
	}
}

// entityStoreSentinel is an unexported singleton used as a "build
// me later" marker — New() detects this exact pointer and replaces
// it with a real IndexEntityStore once it has the backing index. We
// reuse the EntityStore interface (rather than introducing a config
// boolean) so a future caller that wants to inject a custom store
// can set c.entityStore to their own implementation through a
// future WithCustomEntityStore option without churning the field
// list.
var entityStoreSentinel EntityStore = sentinelEntityStore{}

// sentinelEntityStore satisfies EntityStore so the field can hold
// it before New() rewires; its methods are intentionally no-ops to
// avoid masking a wire-up bug behind a partially functional store.
type sentinelEntityStore struct{}

func (sentinelEntityStore) Link(context.Context, Scope, map[string][]string) error {
	return nil
}
func (sentinelEntityStore) Lookup(context.Context, Scope, []string, int) ([]string, error) {
	return nil, nil
}
func (sentinelEntityStore) Forget(context.Context, Scope, string) error { return nil }

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
// supply a persistent adapter (e.g. a SQLite-backed queue) from an
// external package.
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
	pipe      *basepipe.Pipeline
	stopCh    chan struct{}
	wgWorkers sync.WaitGroup

	workerCtx    context.Context
	workerCancel context.CancelFunc

	// reconciler keeps registered side-store [Projection]s
	// eventually consistent with idx. Nil when no projection was
	// registered. Lifecycle is tied to (start in [New], stop in
	// [Close]).
	reconciler *Reconciler

	// projections is the same slice the reconciler holds, exposed
	// here so eager write paths (upsertFacts, Add) can fan out
	// the just-upserted entries to every registered projection
	// via [lt.projectEager] without going through the reconciler.
	// Both the eager path and the reconciler call [Projection.
	// Project] with the same additive semantics; the reconciler
	// adds the diff+Forget step the eager path skips. Wiring
	// landed via #179.1.
	projections []Projection

	// historyAppendMu serialises the read-modify-write fallback in
	// [lt.appendHistory] on a per-namespace basis. Only the
	// fallback path uses it; stores that implement
	// [history.MessageAppender] are expected to provide their own
	// atomicity. Fixes the silent-message-drop race in #154 for
	// third-party stores that don't (yet) implement
	// MessageAppender.
	historyAppendMu syncx.KeyedMutex
}

var (
	_ Memory          = (*lt)(nil)
	_ Auditable       = (*lt)(nil)
	_ JobController   = (*lt)(nil)
	_ SideStoreSyncer = (*lt)(nil)
)

// SyncSideStores implements [SideStoreSyncer]. No-op when no
// projection is registered (the EntityStore was not enabled).
func (m *lt) SyncSideStores(ctx context.Context, scope Scope) error {
	if m.reconciler == nil {
		return nil
	}
	if err := m.validateScope(scope); err != nil {
		return err
	}
	return m.reconciler.SyncScope(ctx, scope)
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
	if len(cfg.ltmOpts) > 0 && len(cfg.legacyLTMOpts) > 0 {
		return nil, errors.New("recall: cannot mix sdk/recall/pipeline and deprecated sdk/retrieval/pipeline LTM options")
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
	// Always initialise the in-memory namespace registry. It is
	// cheap (a sync.Map plus a string slice) and removes the
	// invariant "nsRegistry is nil unless WithSweeper or
	// WithNamespaceRegistry is used" — that invariant was the
	// trigger for #160 (SweepOnce nil-pointer panic when callers
	// drove TTL passes from their own scheduler). Sweeper /
	// projections / SweepOnce / rememberNamespace all now share
	// one registry by default.
	if cfg.nsRegistry == nil {
		cfg.nsRegistry = NewMemoryNamespaceRegistry()
	}
	wrapped := idx
	if cfg.journal != nil {
		wrapped = journal.Wrap(idx, cfg.journal)
	}
	// Construct the entity store now that we have a concrete index.
	// WithEntityStore stashed a sentinel marker; replace it with the
	// IndexEntityStore (or with nil, keeping the feature disabled,
	// when the index can't satisfy DocGetter — NewIndexEntityStore
	// signals this by returning nil).
	if _, isSentinel := cfg.entityStore.(sentinelEntityStore); isSentinel {
		// Audit the explicit-disable opt-out at construction time
		// (es-default). The store's NewIndexEntityStore normalises
		// negative to 0 (gate off) without complaint; the warning
		// only fires when the caller went out of their way to pass
		// a negative — silent default → safe gate stays silent.
		if cfg.entityStoreMaxLinkedCntExplicit && cfg.entityStoreMaxLinkedCnt < 0 && cfg.logger != nil {
			cfg.logger("recall: WithEntityStoreMaxLinkedCount(%d) explicitly disables the common-noun pollution gate; "+
				"this is only safe when --dump-recall histograms confirm your corpus's MetaEntityCount distribution "+
				"is gate-tolerant", cfg.entityStoreMaxLinkedCnt)
		}
		es := NewIndexEntityStore(idx, IndexEntityStoreOptions{
			LinkedCap:      cfg.entityStoreLinkedCap,
			MaxLinkedCount: cfg.entityStoreMaxLinkedCnt,
			Clock:          cfg.now,
		})
		// Assign as an explicit nil interface (not a typed-nil
		// wrapper) when the index doesn't satisfy DocGetter so
		// downstream `if cfg.entityStore == nil` checks behave.
		if es == nil {
			cfg.entityStore = nil
		} else {
			cfg.entityStore = es
		}
	}
	pipe := cfg.pipe
	if pipe == nil {
		// Compose: user-supplied LTM tuning (cfg.ltmOpts) FIRST,
		// auto-wired feature options LAST. Later options win in
		// pipeline.LTM so this gives features the last word —
		// callers who want to OVERRIDE a feature's auto-wire (e.g.
		// turn off the entity-link lane while keeping the store
		// for write-path Link telemetry) should reach for
		// [WithPipeline] explicitly. Within "I want LTM" the
		// recall package owns the final composition.
		if len(cfg.legacyLTMOpts) > 0 {
			legacyOpts := append([]basepipe.LTMOption(nil), cfg.legacyLTMOpts...)
			if cfg.entityStore != nil {
				legacyOpts = append(legacyOpts,
					basepipe.WithMultiRecall(true),
					basepipe.WithEntityLinkLane(true),
					basepipe.WithEntityLinkResolver(newInternalEntityLinkResolver(cfg.entityStore)),
				)
			}
			if cfg.queryEntityLLM != nil {
				legacyOpts = append(legacyOpts,
					basepipe.WithEntityExtractor(llmQueryEntityExtractor(cfg.queryEntityLLM)),
				)
			}
			pipe = basepipe.LTM(cfg.embedder, legacyOpts...)
		} else {
			ltmOpts := append([]recallpipe.LTMOption(nil), cfg.ltmOpts...)
			if cfg.entityStore != nil {
				// The entity-link lane only participates under
				// multi-recall (it's a 4th MultiRetrieve mode);
				// enabling it without multi-recall would silently
				// no-op. Force multi-recall on too so
				// [WithEntityStore] has a single, predictable
				// activation contract: opt in once and both the
				// write path (Link) and the read path (lane + RRF
				// fusion) light up together.
				ltmOpts = append(ltmOpts,
					recallpipe.WithMultiRecall(true),
					recallpipe.WithEntityLinkLane(true),
					recallpipe.WithEntityLinkResolver(newInternalEntityLinkResolver(cfg.entityStore)),
				)
			}
			if cfg.queryEntityLLM != nil {
				// Wire the LLM-backed query entity extractor LAST so
				// it overrides any rule-based extractor a caller may
				// have set via WithLTMOption. Cost: 1 LLM call per
				// recall. Best-effort: errors fall back to "no
				// entities" rather than failing the recall.
				ltmOpts = append(ltmOpts,
					recallpipe.WithEntityExtractor(llmQueryEntityExtractor(cfg.queryEntityLLM)),
				)
			}
			pipe = recallpipe.LTM(cfg.embedder, ltmOpts...)
		}
	} else if len(cfg.ltmOpts) > 0 || len(cfg.legacyLTMOpts) > 0 {
		// User passed both WithPipeline AND WithLTMOption — the
		// latter is dead in this combination. Log so accidental
		// double-wiring shows up in CI rather than silently
		// degrades a feature.
		if cfg.logger != nil {
			cfg.logger("recall: WithLTMOption ignored because WithPipeline is set; pass tuning options via the pipeline construction instead")
		}
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
	// Wire side-store projections. The EntityStore — when enabled
	// via [WithEntityStore] — is the only projection today. New
	// projections (GraphStore, SearchAnalytics, …) plug in by
	// adding an [entityStoreProjection]-style adapter here. The
	// [Reconciler] takes ownership of keeping them in sync with
	// the primary index; write paths stay uninstrumented.
	var projections []Projection
	if cfg.entityStore != nil {
		if pr := newEntityStoreProjection(cfg.entityStore, idx); pr != nil {
			projections = append(projections, pr)
		}
	}
	// Reconciler needs the namespace registry to know which scopes
	// to walk. Construct one on demand if no other component (TTL
	// sweeper) already required it — the registry is cheap and
	// projections without a registry would be unable to find
	// scopes to reconcile. The default-init above already ensures
	// nsRegistry is non-nil, but the guard is kept defensively in
	// case a future refactor revisits the default.
	if len(projections) > 0 && cfg.nsRegistry == nil {
		cfg.nsRegistry = NewMemoryNamespaceRegistry()
	}
	m.projections = projections
	if len(projections) > 0 {
		// The reconciler is created whenever a projection exists so
		// the synchronous [Memory.SyncSideStores] path works in all
		// modes — including [WithReconcileInterval](-1), which only
		// disables the BACKGROUND ticker (see reconciler.start()
		// below).
		m.reconciler = newReconciler(
			wrapped,
			projections,
			cfg.nsRegistry,
			cfg.reconcileInterval,
			cfg.now,
			cfg.logger,
		)
	}
	for i := 0; i < cfg.asyncWorkers; i++ {
		m.wgWorkers.Add(1)
		go m.worker()
	}
	if cfg.sweeperEnabled && cfg.ttlPolicy != nil {
		m.wgWorkers.Add(1)
		go m.sweeperLoop()
	}
	if m.reconciler != nil && cfg.reconcileInterval >= 0 {
		// Negative interval disables the background loop; the
		// synchronous [Memory.SyncSideStores] entry point keeps
		// working (e.g. for tests, deterministic schedulers).
		m.reconciler.start()
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
	if timeout <= 0 {
		s, err := m.JobStatus(ctx, id)
		if err != nil {
			return JobStatus{}, err
		}
		return s, ErrAwaitTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		s, err := m.JobStatus(ctx, id)
		if err != nil {
			return JobStatus{}, err
		}
		switch s.State {
		case JobSucceeded, JobFailed, JobDead:
			return s, nil
		}
		select {
		case <-ctx.Done():
			return s, errdefs.FromContext(ctx.Err())
		case <-timer.C:
			return s, ErrAwaitTimeout
		case <-ticker.C:
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
	// Stop the background reconciler before draining other workers
	// so a long namespace scan does not extend Close beyond its
	// expected budget. The reconciler's loop honours its own stop
	// channel and the per-tick context derived from the loop body.
	if m.reconciler != nil {
		m.reconciler.stop()
	}
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
